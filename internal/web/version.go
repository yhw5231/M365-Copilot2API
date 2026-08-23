package web

import (
	"encoding/json"
	"fmt"
	"m365-copilot2api/internal/outbound"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"
)

var (
	Version     = "0.5.0"
	Commit      = "unknown"
	BuildTime   = "unknown"
	startedAt   = time.Now()
	updateCheck uint32
)

func (s *Server) version(w http.ResponseWriter, _ *http.Request) {
	jsonOut(w, map[string]any{"version": Version, "commit": Commit, "buildTime": BuildTime, "go": runtime.Version(), "uptimeSeconds": int(time.Since(startedAt).Seconds()), "accounts": len(s.tokens.List()), "proxyPool": len(outbound.ProxyPoolStatus()), "persist": PersistFailureStats()})
}

const latestReleaseURL = "https://api.github.com/repos/yhw5231/m365-copilot2api/releases/latest"

type githubRelease struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
}

func (s *Server) update(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	current := normalizedVersion(Version)
	stable := current != "" && current != "dev"
	result := map[string]any{
		"current":         Version,
		"channel":         map[bool]string{true: "stable", false: "development"}[stable],
		"updateAvailable": false,
		"recommendUpdate": false,
	}
	if !stable {
		result["message"] = "当前为开发版，无法与稳定发行版进行可靠比较"
		jsonOut(w, result)
		return
	}

	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, latestReleaseURL, nil)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to create update request")
		return
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "m365-copilot2api/"+current)

	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "无法连接版本发布服务")
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", fmt.Sprintf("版本发布服务返回 HTTP %d", response.StatusCode))
		return
	}

	var release githubRelease
	if err := json.NewDecoder(response.Body).Decode(&release); err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "版本发布服务返回了无效数据")
		return
	}
	latest := normalizedVersion(release.TagName)
	if latest == "" {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "最新发行版缺少有效版本号")
		return
	}

	comparison, err := compareVersions(current, latest)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	available := comparison < 0
	result["latest"] = latest
	result["latestTag"] = release.TagName
	result["releaseURL"] = release.HTMLURL
	result["updateAvailable"] = available
	result["recommendUpdate"] = available
	if available {
		result["message"] = fmt.Sprintf("发现新版本 v%s，当前版本为 v%s", latest, current)
	} else {
		result["message"] = fmt.Sprintf("当前已是最新版本 v%s", current)
	}
	jsonOut(w, result)
}

func normalizedVersion(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "stable-")
	value = strings.TrimPrefix(value, "v")
	return value
}

func compareVersions(left, right string) (int, error) {
	parse := func(value string) ([3]int, error) {
		var parts [3]int
		segments := strings.Split(normalizedVersion(value), ".")
		if len(segments) != len(parts) {
			return parts, fmt.Errorf("无效的版本号 %q", value)
		}
		for index, segment := range segments {
			number, err := strconv.Atoi(segment)
			if err != nil || number < 0 {
				return parts, fmt.Errorf("无效的版本号 %q", value)
			}
			parts[index] = number
		}
		return parts, nil
	}

	leftParts, err := parse(left)
	if err != nil {
		return 0, err
	}
	rightParts, err := parse(right)
	if err != nil {
		return 0, err
	}
	for index := range leftParts {
		if leftParts[index] < rightParts[index] {
			return -1, nil
		}
		if leftParts[index] > rightParts[index] {
			return 1, nil
		}
	}
	return 0, nil
}

func ReleaseTag() string {
	v := strings.TrimSpace(Version)
	if v == "" || v == "dev" {
		return ""
	}
	return fmt.Sprintf("stable-v%s", v)
}
