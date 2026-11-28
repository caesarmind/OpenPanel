package cache

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"gist.github.com/stefanpejcic/openpanel/internal/core/i18n"
)

func TestVarnishBanExprs(t *testing.T) {
	got := varnishBanExprs("example.com", "")
	want := [][]string{
		{"obj.http.x-host", "==", "example.com"},
		{"obj.http.x-host", "==", "www.example.com"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("whole domain: got %v", got)
	}

	got = varnishBanExprs("example.com", "blog.v2")
	if len(got) != 2 || got[0][len(got[0])-1] != `^/blog\.v2(/|$)` {
		t.Fatalf("folder: got %v", got)
	}
}

func TestWPRedisPrefix(t *testing.T) {
	for site, want := range map[string]string{
		"example.com":      "op_example_com:",
		"Example.com/Blog": "op_example_com_blog:",
	} {
		if got := WPRedisPrefix(site); got != want {
			t.Errorf("%s: got %q, want %q", site, got, want)
		}
	}
}

func TestPurgePlan(t *testing.T) {
	all := purgeFacts{PHPContainer: "php-fpm-8.3", Docroot: "/var/www/html/example.com", VarnishRunning: true, VarnishDomain: "On", WPSites: []string{"example.com"}}
	if v, o, c := purgePlan(all); v != "" || o != "" || c != "" {
		t.Fatalf("everything on should run all layers, got %q %q %q", v, o, c)
	}

	cases := []struct {
		name    string
		change  func(*purgeFacts)
		v, o, c bool // true when the layer is skipped
	}{
		{"varnish stopped", func(f *purgeFacts) { f.VarnishRunning = false }, true, false, false},
		{"varnish off for domain", func(f *purgeFacts) { f.VarnishDomain = "Off" }, true, false, false},
		{"litespeed", func(f *purgeFacts) { f.Litespeed = true }, false, true, false},
		{"no php", func(f *purgeFacts) { f.PHPContainer = "" }, false, true, true},
		{"no docroot", func(f *purgeFacts) { f.Docroot = "" }, false, true, false},
		{"no wordpress", func(f *purgeFacts) { f.WPSites = nil }, false, false, true},
	}
	for _, tc := range cases {
		f := all
		tc.change(&f)
		v, o, c := purgePlan(f)
		if (v != "") != tc.v || (o != "") != tc.o || (c != "") != tc.c {
			t.Errorf("%s: got varnish=%q opcache=%q object=%q", tc.name, v, o, c)
		}
	}
}

func TestPurgeResultMessage(t *testing.T) {
	tr := i18n.NewManager(t.TempDir(), nil).Translator("en")

	full := PurgeResult{Domain: "example.com", Layers: []PurgeLayer{
		{Name: "varnish", State: PurgeDone},
		{Name: "opcache", State: PurgeDone, Count: 214},
		{Name: "object_cache", State: PurgeDone, Count: 1},
	}}
	if got, want := full.Message(tr), "Cache purged for example.com: Varnish, OPcache (214 scripts), WordPress object cache (1 site)."; got != want {
		t.Errorf("full: got %q", got)
	}
	if !full.Purged() || full.Failed() || strings.Join(full.Names(), ",") != "varnish,opcache,object_cache" {
		t.Errorf("full: wrong flags or names %v", full.Names())
	}

	shared := PurgeResult{Domain: "example.com", Layers: []PurgeLayer{
		skipped("varnish", "Varnish is not running"),
		{Name: "opcache", State: PurgeDone, Shared: true},
	}}
	if got, want := shared.Message(tr), "Cache purged for example.com: OPcache (all sites on this PHP version)."; got != want {
		t.Errorf("shared: got %q", got)
	}

	none := PurgeResult{Domain: "example.com", Layers: []PurgeLayer{
		skipped("varnish", "Varnish is not running"),
		skipped("opcache", "The domain has no PHP container"),
	}}
	if got, want := none.Message(tr), "Nothing to purge for example.com. Varnish is not running. The domain has no PHP container."; got != want {
		t.Errorf("none: got %q", got)
	}
	if none.Purged() {
		t.Error("none: Purged should be false")
	}

	failed := PurgeResult{Domain: "example.com", Layers: []PurgeLayer{
		{Name: "varnish", State: PurgeFailed, Detail: "exit status 1"},
		{Name: "opcache", State: PurgeDone, Count: 3},
	}}
	if got := failed.Message(tr); !strings.Contains(got, "OPcache (3 scripts)") || !strings.Contains(got, "Varnish: exit status 1") {
		t.Errorf("failed: got %q", got)
	}
}

func TestClaimFPMReload(t *testing.T) {
	now := time.Now()
	key := "user-" + t.Name() + "/php-fpm-8.3"
	if !claimFPMReload(key, now) {
		t.Fatal("first reload should be allowed")
	}
	if claimFPMReload(key, now.Add(fpmReloadGap/2)) {
		t.Error("second reload inside the gap should be refused")
	}
	if !claimFPMReload(key+"-other", now) {
		t.Error("another container should not be held back")
	}
	if !claimFPMReload(key, now.Add(fpmReloadGap+time.Second)) {
		t.Error("reload after the gap should be allowed")
	}
}
