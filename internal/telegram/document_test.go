package telegram

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/maniack/tgaia/internal/config"
)

func TestSendDocumentUsesMultipart(t *testing.T) {
	client := New(config.NewSecret("test-token"))
	var fields map[string]string
	var fileName, fileBody string
	client.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(request.URL.Path, "/sendDocument") {
			t.Fatalf("unexpected method %s", request.URL.Path)
		}
		_, params, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
		if err != nil {
			t.Fatal(err)
		}
		reader := multipart.NewReader(request.Body, params["boundary"])
		fields = map[string]string{}
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			data, _ := io.ReadAll(part)
			if part.FormName() == "document" {
				fileName, fileBody = part.FileName(), string(data)
				continue
			}
			fields[part.FormName()] = string(data)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":9,"date":1}}`)), Header: make(http.Header)}, nil
	})
	id, _, err := client.SendDocument(context.Background(), 42, 7, "reply.html", []byte("<html></html>"), "**Игра** готова")
	if err != nil || id != 9 {
		t.Fatal(err, id)
	}
	if fields["chat_id"] != "42" || fields["message_thread_id"] != "7" || fields["parse_mode"] != "HTML" || fields["caption"] != "<b>Игра</b> готова" {
		t.Fatalf("unexpected fields %#v", fields)
	}
	if fileName != "reply.html" || fileBody != "<html></html>" {
		t.Fatalf("unexpected file %q %q", fileName, fileBody)
	}
	if _, _, err := client.SendDocument(context.Background(), 42, 0, "../x", []byte("a"), ""); err == nil {
		t.Fatal("path-like names must be rejected")
	}
}

func TestRejectedResponsesAreDistinguishable(t *testing.T) {
	client := New(config.NewSecret("test-token"))
	status := 403
	client.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"ok":false,"description":"Forbidden: not enough rights"}`)), Header: make(http.Header)}, nil
	})
	var rejected interface{ Rejected() bool }
	_, _, err := client.SendDocument(context.Background(), 1, 0, "a.txt", []byte("a"), "")
	if !errors.As(err, &rejected) || strings.Contains(err.Error(), "rights") {
		t.Fatalf("4xx must be a rejection without echoing the description: %v", err)
	}
	status = 500
	_, _, err = client.Send(context.Background(), 1, "x")
	if errors.As(err, &rejected) {
		t.Fatal("5xx is an unknown outcome, not a rejection")
	}
}
