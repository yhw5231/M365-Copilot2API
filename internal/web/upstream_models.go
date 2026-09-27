package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/outbound"
)

// The routing console shows one row per public model and routes it at an
// upstream mapping (ChatHub tone). Which tones exist upstream changes whenever
// Microsoft ships or retires a model, so the console can re-read the list on
// demand.
//
// Three sources are enumerated and unioned, because no single one is complete:
//
//   - the account's model picker catalog, embedded in the server-side rendered
//     /chat document (availableModelSelectionOptions). It is per-tenant and
//     carries the newest models (GPT 5.6 showed up here first), but it is a UI
//     surface: it stops listing older generations that ChatHub still serves.
//   - the app's own tone table, from the script chunks the document preloads
//     (Gpt_5_2_Chat … Claude_Sonnet). That table is broader and older.
//   - the built-in known-good list (knownUpstreamTones), which records tones
//     observed to work on tenants whose picker no longer mentions them, e.g.
//     Gpt_5_5_Chat.
//
// A mapping is therefore only pruned when no source lists it at all.
//
// The app document is only served to a token minted for the web app audience,
// so the refresh borrows one authorized account and mints that scope from its
// refresh token.
//
// The earlier implementation scraped https://m365.cloud.microsoft/ for a
// "main.<hash>.js" reference. That page now answers anonymous requests with a
// 79-byte stub and never names a bundle, so the list silently stayed empty and
// only the built-in defaults were ever offered.
const (
	// upstreamWebScope is the audience of the M365 Copilot web app.
	upstreamWebScope = "https://m365.cloud.microsoft/v2/.default"
	// upstreamAppURL is the Copilot chat route; its document carries the model
	// picker catalog and the chunk list.
	upstreamAppURL = "https://m365.cloud.microsoft/chat"
	// upstreamChunkBase hosts the app chunks referenced by that document.
	upstreamChunkBase = "https://res.public.onecdn.static.microsoft/midgard/versionless-v2/"
	// upstreamWebUserAgent mimics the browser the app expects; the SSR
	// endpoints answer unknown clients with the anonymous landing page.
	upstreamWebUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	// upstreamPickerKey anchors the picker catalog inside the document. The
	// embedded state is JSON escaped into a script string, so the key is
	// matched after unescaping.
	upstreamPickerKey = `"availableModelSelectionOptions":`

	// upstreamSyncTimeout bounds one refresh end to end (token, document and
	// every chunk), so the console's button cannot hang on a slow CDN.
	upstreamSyncTimeout = 60 * time.Second
	// upstreamMaxChunks caps how many chunks one refresh downloads.
	upstreamMaxChunks = 48
	// upstreamMaxChunkBytes caps a single chunk read.
	upstreamMaxChunkBytes = 12 << 20
	// upstreamChunkWorkers is the chunk download fan-out.
	upstreamChunkWorkers = 6
	// upstreamAccountAttempts is how many accounts one refresh tries before
	// giving up; a single stale refresh token must not fail the whole sync.
	upstreamAccountAttempts = 3
)

// upstreamChunkRef matches the chunk file names the app document references.
var upstreamChunkRef = regexp.MustCompile(`versionless-v2/([A-Za-z0-9_.\-]+\.js)`)

// upstreamTonePattern matches ChatHub tone identifiers inside a chunk. Tone
// names follow "<Family>_<version parts>" (Gpt_5_6_Reasoning, Claude_Sonnet).
var upstreamTonePattern = regexp.MustCompile(`\b(?:Gpt_[0-9]+_[0-9]+_[A-Za-z_]+|Claude_[A-Za-z0-9_]+)\b`)

// upstreamToneID constrains the picker catalog ids that are usable as route
// targets: a tone id carries a family and a variant ("Gpt_5_6_Reasoning",
// "Claude_Sonnet"). The catalog also lists the app's own generic modes
// ("Chat", "Reasoning") and the "Magic" auto placeholder — those are not
// ChatHub tones the gateway routes to, and a bare "Reasoning" would even be
// rewritten into the console's own label by the UI dictionary.
var upstreamToneID = regexp.MustCompile(`^[A-Za-z0-9]+_[A-Za-z0-9_]+$`)

// upstreamModelFetcher reads the live upstream model list. It is a variable so
// tests can stub the account/token/network plumbing.
var upstreamModelFetcher = func(ctx context.Context, s *Server) ([]string, error) {
	return s.fetchUpstreamModels(ctx)
}

// upstreamAccountFetcher reads the remote sources through one account; it is a
// variable so tests can exercise the union with the built-in list offline.
var upstreamAccountFetcher = func(ctx context.Context, s *Server, acc auth.AccountToken) ([]string, error) {
	return s.fetchUpstreamModelsWithAccount(ctx, acc)
}

// upstreamAppChunks lists the chunk files referenced by the app document.
func upstreamAppChunks(doc []byte) []string {
	matches := upstreamChunkRef.FindAllSubmatch(doc, -1)
	seen := make(map[string]bool, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		name := string(m[1])
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// scanUpstreamTones collects the tone identifiers found in one chunk body.
//
// "Magic" is deliberately dropped: the gateway treats it as its own
// placeholder/fallback tone (see modelRouteTable), never as a route target, and
// the built-in fallback list omits it for the same reason.
func scanUpstreamTones(body []byte) []string {
	matches := upstreamTonePattern.FindAll(body, -1)
	seen := make(map[string]bool, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		tone := strings.TrimSpace(string(m))
		if tone == "" || strings.EqualFold(tone, "Magic") {
			continue
		}
		key := strings.ToLower(tone)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, tone)
	}
	sort.Strings(out)
	return out
}

// parseUpstreamModelIDs reads the model picker catalog out of the app document.
// It returns the ids the signed-in account may actually pick (flattened out of
// the picker's item groups), which is the tenant's authoritative model list.
//
// The embedded state is JSON escaped into a script string, so the document is
// unescaped first and every catalog occurrence is decoded on its own: the
// payload appears once per preloaded scenario and a malformed one must not
// discard the others.
func parseUpstreamModelIDs(doc []byte) []string {
	text := strings.ReplaceAll(string(doc), `\"`, `"`)
	seen := map[string]bool{}
	ids := make([]string, 0, 8)
	for pos := 0; pos < len(text); {
		i := strings.Index(text[pos:], upstreamPickerKey)
		if i < 0 {
			break
		}
		start := pos + i + len(upstreamPickerKey)
		pos = start
		var items []map[string]any
		if err := json.NewDecoder(strings.NewReader(text[start:])).Decode(&items); err != nil {
			continue
		}
		collectPickerIDs(items, seen, &ids)
	}
	sort.Strings(ids)
	return ids
}

// collectPickerIDs flattens a picker catalog: entries either carry the model id
// directly or group the models of one family ("OpenAI").
func collectPickerIDs(items []map[string]any, seen map[string]bool, out *[]string) {
	for _, item := range items {
		if group, ok := item["itemGroup"]; ok {
			nested := make([]map[string]any, 0, 4)
			if raw, ok := group.([]any); ok {
				for _, entry := range raw {
					if m, ok := entry.(map[string]any); ok {
						nested = append(nested, m)
					}
				}
			}
			collectPickerIDs(nested, seen, out)
			continue
		}
		id, _ := item["id"].(string)
		id = strings.TrimSpace(id)
		if !upstreamToneID.MatchString(id) {
			continue
		}
		key := strings.ToLower(id)
		if seen[key] {
			continue
		}
		seen[key] = true
		*out = append(*out, id)
	}
}

// referencedMappingNames lists the upstream mapping names the model routes
// point at (lower-cased). A refresh must never delete one of these: the
// settings validator rejects a route whose mapping is missing.
func referencedMappingNames(mappings []modelMapping) map[string]bool {
	referenced := make(map[string]bool, len(mappings))
	for _, m := range mappings {
		if name := strings.ToLower(strings.TrimSpace(m.UpstreamMapping)); name != "" {
			referenced[name] = true
		}
	}
	return referenced
}

// syncUpstreamMappings rewrites the upstream mapping set from a freshly read
// model list — a refresh is a force update, not a merge:
//
//   - a mapping the list still carries keeps its tone string and its enabled
//     switch (matching by tone, falling back to the mapping name),
//   - a mapping the list no longer carries is deleted,
//   - a model the set does not carry yet is appended with tone = name (the
//     mapping name is what gets sent to ChatHub).
//
// The one exception is a mapping a model route still points at: deleting it
// would make the settings unsavable, so it is kept and reported as unavailable.
// The caller decides how complete "the list" is — see fetchUpstreamModels,
// which unions every source before calling this.
func syncUpstreamMappings(current []upstreamMapping, available []string, referenced map[string]bool) (merged []upstreamMapping, added, removed, unavailable []string) {
	canonical := make(map[string]string, len(available))
	for _, id := range available {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		canonical[strings.ToLower(id)] = id
	}
	kept := make(map[string]bool, len(current)+len(canonical))
	merged = make([]upstreamMapping, 0, len(current)+len(canonical))
	for _, m := range current {
		name := strings.TrimSpace(m.Name)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, ok := canonical[strings.ToLower(strings.TrimSpace(m.Tone))]; ok {
			merged = append(merged, m)
			kept[key] = true
			continue
		}
		if _, ok := canonical[key]; ok {
			merged = append(merged, m)
			kept[key] = true
			continue
		}
		if referenced[key] {
			merged = append(merged, m)
			kept[key] = true
			unavailable = append(unavailable, name)
			continue
		}
		removed = append(removed, name)
	}
	added = make([]string, 0, len(canonical))
	for _, id := range available {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		if kept[key] {
			continue
		}
		kept[key] = true
		merged = append(merged, upstreamMapping{Name: id, Tone: id})
		added = append(added, id)
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(unavailable)
	return merged, added, removed, unavailable
}

// upstreamAccountPrefs orders the account pool for a refresh: usable online
// accounts first, so a single stale token does not fail the sync.
func upstreamAccounts(accounts []auth.AccountToken) []auth.AccountToken {
	ordered := make([]auth.AccountToken, 0, len(accounts))
	for _, pass := range []func(auth.AccountToken) bool{
		func(a auth.AccountToken) bool { return a.Status == "online" },
		func(a auth.AccountToken) bool { return a.Status != "online" },
	} {
		for _, acc := range accounts {
			if strings.TrimSpace(acc.RefreshToken) == "" || !pass(acc) {
				continue
			}
			ordered = append(ordered, acc)
		}
	}
	return ordered
}

// fetchUpstreamModels reads the upstream model list, borrowing an authorized
// account for the web-app token. The result is the union of every source the
// gateway can enumerate: what the account's picker currently offers, the app's
// own tone table, and the built-in known-good list. ChatHub keeps accepting
// older tones long after the picker stops showing them (a tenant whose picker
// only lists GPT 5.6 still answers on Gpt_5_5_Chat), so a list built from one
// source alone would be wrong in both directions.
func (s *Server) fetchUpstreamModels(ctx context.Context) ([]string, error) {
	accounts := upstreamAccounts(s.tokens.List())
	if len(accounts) == 0 {
		return nil, errors.New("没有可用账号：拉取上游模型列表需要一个已授权账号")
	}
	var lastErr error
	for i, acc := range accounts {
		if i >= upstreamAccountAttempts {
			break
		}
		models, err := upstreamAccountFetcher(ctx, s, acc)
		if err == nil {
			return unionUpstreamModels(models, knownUpstreamTones()), nil
		}
		log.Printf("[model-sync] account %s failed: %v", acc.Email, err)
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("上游模型列表为空")
	}
	return nil, lastErr
}

// unionUpstreamModels merges model lists, keeping the first spelling of every
// id (case-insensitive) and sorting the result.
func unionUpstreamModels(lists ...[]string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, 24)
	for _, list := range lists {
		for _, id := range list {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			key := strings.ToLower(id)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func (s *Server) fetchUpstreamModelsWithAccount(ctx context.Context, acc auth.AccountToken) ([]string, error) {
	token, err := s.upstreamWebToken(acc)
	if err != nil {
		return nil, err
	}
	client := *outbound.HTTPClientFor(acc.ID)
	client.Timeout = upstreamSyncTimeout
	doc, err := upstreamGet(ctx, &client, upstreamAppURL, token)
	if err != nil {
		return nil, fmt.Errorf("读取上游应用文档失败: %w", err)
	}
	// The picker catalog is the account's per-tenant offer (it carries the
	// newest models), the chunk tones are the app's full tone table.
	picker := parseUpstreamModelIDs(doc)
	chunks := upstreamAppChunks(doc)
	if len(chunks) > upstreamMaxChunks {
		chunks = chunks[:upstreamMaxChunks]
	}
	catalog := s.scanUpstreamChunks(ctx, &client, chunks)
	if len(picker) == 0 && len(catalog) == 0 {
		if len(chunks) == 0 {
			return nil, errors.New("上游应用文档未列出任何脚本（令牌失效或页面结构变化）")
		}
		return nil, errors.New("上游文档与脚本中均未发现可用模型")
	}
	log.Printf("[model-sync] account %s: %d picker models, %d catalog tones", acc.Email, len(picker), len(catalog))
	return unionUpstreamModels(picker, catalog), nil
}

// scanUpstreamChunks downloads the given chunks with a bounded fan-out and
// unions the tone identifiers they contain. Individual chunk failures are
// tolerated: the model picker lives in a few chunks, and one 404 must not fail
// the refresh (the app document references a locale chunk the CDN does not
// serve).
func (s *Server) scanUpstreamChunks(ctx context.Context, client *http.Client, chunks []string) []string {
	type result struct {
		tones []string
		err   error
		name  string
	}
	results := make([]result, len(chunks))
	slots := make(chan struct{}, upstreamChunkWorkers)
	var wg sync.WaitGroup
	for i, name := range chunks {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			body, err := upstreamGet(ctx, client, upstreamChunkBase+name, "")
			if err != nil {
				results[i] = result{name: name, err: err}
				return
			}
			results[i] = result{name: name, tones: scanUpstreamTones(body)}
		}(i, name)
	}
	wg.Wait()

	seen := map[string]bool{}
	union := make([]string, 0, 16)
	failed := 0
	for _, r := range results {
		if r.err != nil {
			failed++
			log.Printf("[model-sync] chunk %s skipped: %v", r.name, r.err)
			continue
		}
		for _, tone := range r.tones {
			key := strings.ToLower(tone)
			if seen[key] {
				continue
			}
			seen[key] = true
			union = append(union, tone)
		}
	}
	if len(results) > 0 && failed == len(results) {
		log.Printf("[model-sync] all %d chunks failed to download", failed)
	}
	sort.Strings(union)
	return union
}

// upstreamWebToken mints an access token for the M365 web app audience from the
// account's refresh token. A rotated refresh token is persisted so the account
// pool keeps working after the refresh.
func (s *Server) upstreamWebToken(acc auth.AccountToken) (string, error) {
	if strings.TrimSpace(acc.RefreshToken) == "" {
		return "", fmt.Errorf("账号 %s 缺少 refresh token", acc.Email)
	}
	clientID := firstNonEmpty(acc.ClientID, auth.ClientID())
	set, err := auth.RefreshWithScope(acc.RefreshToken, clientID, upstreamWebScope)
	if err != nil {
		return "", fmt.Errorf("获取上游网页令牌失败: %w", err)
	}
	if set.RefreshToken != "" && set.RefreshToken != acc.RefreshToken {
		if err := s.tokens.UpdateRefreshToken(acc.ID, set.RefreshToken); err != nil {
			log.Printf("[model-sync] rotated refresh token not persisted account=%s err=%v", acc.ID, err)
		}
	}
	return set.AccessToken, nil
}

// upstreamGet performs one authorized GET against the upstream/CDN. The token
// is optional (CDN chunks are public).
func upstreamGet(ctx context.Context, client *http.Client, url, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", upstreamWebUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/javascript,*/*")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, upstreamMaxChunkBytes))
	if err != nil {
		return nil, err
	}
	return body, nil
}
