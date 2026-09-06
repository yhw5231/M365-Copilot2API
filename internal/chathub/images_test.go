package chathub

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestImageURLs(t *testing.T) {
	raw := []json.RawMessage{
		json.RawMessage(`{"content":{"image":{"downloadUrl":"https://cdn.example.com/image/1.png","thumbnailUrl":"https://cdn.example.com/image/1.png"}},"url":"https://example.com/page"}`),
		json.RawMessage(`{"src":"https://cdn.example.com/image/2.webp"}`),
	}
	got := imageURLs(raw)
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}

func TestImageURLsImageReferenceUrls(t *testing.T) {
	// Captured live from ChatHub image generation progress message
	// (contentGenerationProgressList[0].ImageReferenceUrls).
	raw := []json.RawMessage{json.RawMessage(`{
	  "type": 1,
	  "target": "update",
	  "arguments": [{
	    "messages": [{
	      "text": "Loading image",
	      "contentGenerationProgressList": [{
	        "contentType": "image",
	        "pollUrl": "eyJQb2xsSWQiOiJjZTRjMjYyZiIsIkZpbGVUb2tlbiI6InRva19oZXJlIn0=",
	        "fileToken": "e4dbaf90-17b5-4025-a4a8-5ff377c7849c",
	        "ImageReferenceUrls": [
	          "https://designerapp.officeapps.live.com/designerapp/document.ashx?path=%2Fe0b90f31-23a9-41cb-b556-8eb15d31b16c%2FDallEGeneratedImages%2Fdalle-a7176d65-397f-475f-a470-81da39f348cd0251616132911800150700.png&dcHint=KoreaCentral&speCId=a899c570-5d1e-49e7-b272-9a5ffc47427c&speType=Image&speIdx=0&fileToken=eyJUb2tlblByZWZpeCI6IkFBRC03In0"
	        ]
	      }],
	      "contentType": "GraphicArt",
	      "contentOrigin": "ImageGeneration"
	    }]
	  }]
	}`)}
	got := imageURLs(raw)
	if len(got) != 1 {
		t.Fatalf("got %v", got)
	}
	if !strings.Contains(got[0], "designerapp.officeapps.live.com") {
		t.Fatalf("unexpected url %q", got[0])
	}
}

func TestImageURLsRejectsUnsafe(t *testing.T) {
	raw := []json.RawMessage{json.RawMessage(`{"url":"http://example.com/a.png"}`)}
	if got := imageURLs(raw); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestCapImageAttachmentsTruncatesOldest(t *testing.T) {
	// A session-miss transcript replay: 13 images plus one file attachment.
	req := &Request{Text: "answer the question"}
	for i := 1; i <= 13; i++ {
		req.Attachments = append(req.Attachments, Attachment{Type: "image", URL: fmt.Sprintf("data:image/png;base64,img-%02d", i)})
	}
	req.Attachments = append(req.Attachments,
		Attachment{Type: "file", URL: "data:text/plain;base64,doc", Name: "doc.txt"},
		Attachment{Type: "image", URL: "data:image/png;base64,img-14"},
	)
	(&Client{}).capImageAttachments(req)

	images, files := 0, 0
	for _, a := range req.Attachments {
		switch a.Type {
		case "image":
			images++
		case "file":
			files++
		}
	}
	if images != maxAttachments {
		t.Fatalf("images kept = %d, want %d", images, maxAttachments)
	}
	if files != 1 {
		t.Fatalf("file attachments lost: got %d, want 1", files)
	}
	// The OLDEST images are dropped; the newest (img-05..img-14) stay, in order.
	first, last := req.Attachments[0].URL, req.Attachments[len(req.Attachments)-1].URL
	if !strings.Contains(first, "img-05") {
		t.Fatalf("first kept image = %q, want img-05", first)
	}
	if !strings.Contains(last, "img-14") {
		t.Fatalf("last kept image = %q, want img-14", last)
	}
	if !strings.Contains(req.Text, "4 of 14 images were omitted") {
		t.Fatalf("prompt notice missing: %q", req.Text)
	}
	if !strings.HasPrefix(req.Text, "answer the question") {
		t.Fatalf("original prompt altered: %q", req.Text)
	}
}

func TestCapImageAttachmentsUnderLimit(t *testing.T) {
	req := &Request{Text: "hi"}
	for i := 1; i <= maxAttachments; i++ {
		req.Attachments = append(req.Attachments, Attachment{Type: "image", URL: fmt.Sprintf("data:image/png;base64,img-%02d", i)})
	}
	orig := len(req.Attachments)
	(&Client{}).capImageAttachments(req)
	if len(req.Attachments) != orig {
		t.Fatalf("attachments changed: got %d, want %d", len(req.Attachments), orig)
	}
	if req.Text != "hi" {
		t.Fatalf("notice appended under limit: %q", req.Text)
	}
}
