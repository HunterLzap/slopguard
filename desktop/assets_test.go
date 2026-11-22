//go:build windows || darwin

package main

import (
	"bytes"
	"image/png"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/wailsapp/wails/v3/pkg/application"
	"golang.org/x/net/html"
)

func TestEmbeddedPanelAssets(t *testing.T) {
	handler, err := panelAssets()
	if err != nil {
		t.Fatal(err)
	}
	for _, asset := range []struct{ path, contentType, needle string }{
		{"/", "text/html", "<title>SlopGuard</title>"},
		{"/styles.css", "text/css", "prefers-color-scheme: dark"},
		{"/app.js", "javascript", "/wails/runtime.js"},
		{"/slopguard.png", "image/png", "PNG"},
	} {
		t.Run(asset.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, asset.path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), asset.contentType) ||
				!bytes.Contains(rec.Body.Bytes(), []byte(asset.needle)) {
				t.Fatalf("asset %s: %d %q, content length %d", asset.path, rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
			}
		})
	}
	for _, icon := range [][]byte{appIcon, trayIcon, pendingIcon} {
		if _, err := png.Decode(bytes.NewReader(icon)); err != nil {
			t.Fatal(err)
		}
	}
	if bytes.Equal(trayIcon, pendingIcon) {
		t.Fatal("pending tray icon is identical")
	}
}

func TestPanelDOMAndBindings(t *testing.T) {
	page, err := frontend.ReadFile("frontend/index.html")
	if err != nil {
		t.Fatal(err)
	}
	root, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*html.Node{}
	var mainSections []*html.Node
	for node := range root.Descendants() {
		for _, a := range node.Attr {
			if a.Key == "id" {
				if byID[a.Val] != nil {
					t.Fatalf("duplicate ID %q", a.Val)
				}
				byID[a.Val] = node
			}
		}
		if node.Data == "section" {
			mainSections = append(mainSections, node)
		}
	}
	idList := []string{"topbar", "guard-state", "open-settings", "scroller", "proposal-section", "proposals", "agents", "projects",
		"copy-mcp", "copy-json", "copy-toml", "add-project", "settings-drawer", "close-settings", "settings-form",
		"base-url", "api-key", "show-key", "model", "autostart", "settings-message", "test-connection", "save-settings",
		"toast", "confirm-rule", "confirm-name"}
	for _, id := range idList {
		if byID[id] == nil {
			t.Fatalf("missing DOM element %q", id)
		}
	}
	if len(mainSections) != 3 || mainSections[0] != byID["proposal-section"] || !hasAttr(mainSections[0], "hidden", "") ||
		byID["settings-drawer"].Data != "dialog" || !hasAttr(byID["api-key"], "type", "password") {
		t.Fatal("panel structure does not match the single-screen contract")
	}

	script, err := frontend.ReadFile("frontend/app.js")
	if err != nil {
		t.Fatal(err)
	}
	refs := regexp.MustCompile(`\$\("([^"]+)"\)`).FindAllSubmatch(script, -1)
	for _, ref := range refs {
		if byID[string(ref[1])] == nil {
			t.Errorf("missing script DOM target %s", ref[1])
		}
	}
	if len(refs) == 0 {
		t.Fatal("no DOM references discovered")
	}
	var names []string
	calls := regexp.MustCompile(`bind\("([A-Za-z]+)"\)`).FindAllSubmatch(script, -1)
	for _, call := range calls {
		names = append(names, string(call[1]))
	}
	slices.Sort(names)
	want := []string{"ApproveProposal", "DeleteProjectRule", "DismissProposal", "GetDashboardState", "GetGenericMCPSnippets",
		"GetProjectDetails", "GetSettings", "PickAndRegisterProject", "RemoveProject", "SaveSettings", "TestLLMConnection",
		"ToggleAgent", "ToggleProjectHook", "ToggleProjectRule", "VerifyProject"}
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("JS methods = %v; want %v", names, want)
	}
	if !bytes.Contains(script, []byte(`Call.ByName("main.AppService." + method`)) {
		t.Fatal("Wails application binding namespace changed")
	}

	application.New(application.Options{Name: "SlopGuard binding test"})
	bindings := application.NewBindings(nil, nil)
	service := &AppService{}
	if err := bindings.Add(application.NewService(service)); err != nil {
		t.Fatal(err)
	}
	prefix := reflect.TypeOf(service).Elem().PkgPath() + ".AppService."
	for _, name := range names {
		if bindings.Get(&application.CallOptions{MethodName: prefix + name}) == nil {
			t.Errorf("Wails did not bind %s", name)
		}
	}
}

func hasAttr(node *html.Node, key, value string) bool {
	return slices.ContainsFunc(node.Attr, func(attr html.Attribute) bool { return attr.Key == key && attr.Val == value })
}
