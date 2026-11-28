package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	appctx "gist.github.com/stefanpejcic/openpanel/internal/app"
	"gist.github.com/stefanpejcic/openpanel/internal/core/i18n"
	"gist.github.com/stefanpejcic/openpanel/internal/core/podmanmanager"
	"gist.github.com/stefanpejcic/openpanel/internal/core/tenantfs"
	"gist.github.com/stefanpejcic/openpanel/internal/core/webserver"
	"gist.github.com/stefanpejcic/openpanel/internal/modules/docker"
	"gist.github.com/stefanpejcic/openpanel/internal/modules/php"
)

// RedisDropinMarker is the header of the Redis Object Cache plugin's object-cache.php
const RedisDropinMarker = "Redis Object Cache Drop-In"

var (
	purgeDomainRE    = regexp.MustCompile(`^[a-z0-9.-]+$`)
	purgeContainerRE = regexp.MustCompile(`^[a-z0-9._-]+$`)
	purgeFolderRE    = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
	redisPrefixRE    = regexp.MustCompile(`[^a-z0-9]+`)
)

// Purge layer states
const (
	PurgeDone    = "purged"
	PurgeSkipped = "skipped"
	PurgeFailed  = "failed"
)

// PurgeLayer is what happened to one cache layer during a purge
type PurgeLayer struct {
	Name   string `json:"name"` // varnish, opcache or object_cache
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
	// Count is scripts dropped for opcache and WordPress sites flushed for object_cache
	Count int `json:"count,omitempty"`
	// Shared is set when the opcache reset covered every site on that PHP version
	Shared bool `json:"shared,omitempty"`
}

// PurgeResult is the outcome of PurgeDomain
type PurgeResult struct {
	Domain string       `json:"domain"`
	Layers []PurgeLayer `json:"layers"`
}

// Purged reports whether at least one layer was cleared
func (p PurgeResult) Purged() bool {
	for _, l := range p.Layers {
		if l.State == PurgeDone {
			return true
		}
	}
	return false
}

// Failed reports whether a layer was tried and failed
func (p PurgeResult) Failed() bool {
	for _, l := range p.Layers {
		if l.State == PurgeFailed {
			return true
		}
	}
	return false
}

// Names lists the cleared layers for the activity log
func (p PurgeResult) Names() []string {
	var out []string
	for _, l := range p.Layers {
		if l.State == PurgeDone {
			out = append(out, l.Name)
		}
	}
	return out
}

// Message is the one-line summary shown to the user
func (p PurgeResult) Message(t i18n.Translator) string {
	var done, rest []string
	for _, l := range p.Layers {
		label := purgeLayerLabel(t, l)
		switch l.State {
		case PurgeDone:
			done = append(done, label)
		case PurgeFailed:
			rest = append(rest, label+": "+l.Detail)
		default:
			if l.Detail != "" {
				rest = append(rest, t.Get(l.Detail))
			}
		}
	}
	if len(done) == 0 {
		msg := t.Get("Nothing to purge for %(domain)s.", "domain", p.Domain)
		if len(rest) > 0 {
			msg += " " + strings.Join(rest, ". ") + "."
		}
		return msg
	}
	msg := t.Get("Cache purged for %(domain)s: %(layers)s.", "domain", p.Domain, "layers", strings.Join(done, ", "))
	if p.Failed() {
		msg += " " + strings.Join(rest, ". ") + "."
	}
	return msg
}

func purgeLayerLabel(t i18n.Translator, l PurgeLayer) string {
	switch l.Name {
	case "varnish":
		return "Varnish"
	case "opcache":
		if l.State != PurgeDone {
			return "OPcache"
		}
		if l.Shared {
			return t.Get("OPcache (all sites on this PHP version)")
		}
		return strings.ReplaceAll(t.GetN("OPcache (%(count)s script)", "OPcache (%(count)s scripts)", l.Count), "%(count)s", strconv.Itoa(l.Count))
	case "object_cache":
		if l.State != PurgeDone {
			return t.Get("WordPress object cache")
		}
		return strings.ReplaceAll(t.GetN("WordPress object cache (%(count)s site)", "WordPress object cache (%(count)s sites)", l.Count), "%(count)s", strconv.Itoa(l.Count))
	}
	return l.Name
}

// purgeFacts is what PurgeDomain needs to know before it touches anything
type purgeFacts struct {
	Litespeed      bool
	PHPContainer   string // empty when the domain has no php-fpm container
	Docroot        string // container path
	VarnishRunning bool
	VarnishDomain  string // On, Off or Unknown
	WPSites        []string
}

// purgePlan decides per layer whether it runs, the empty string means run and anything else is the reason to skip
func purgePlan(f purgeFacts) (varnish, opcache, objectCache string) {
	switch {
	case !f.VarnishRunning:
		varnish = "Varnish is not running"
	case f.VarnishDomain != "On":
		varnish = "Varnish is off for this domain"
	}
	switch {
	case f.Litespeed:
		opcache = "OPcache reset is not available on LiteSpeed"
	case f.PHPContainer == "":
		opcache = "The domain has no PHP container"
	case f.Docroot == "":
		opcache = "Document root is unknown"
	}
	switch {
	case len(f.WPSites) == 0:
		objectCache = "No WordPress site on this domain"
	case f.PHPContainer == "":
		objectCache = "The domain has no PHP container"
	}
	return varnish, opcache, objectCache
}

func skipped(name, reason string) PurgeLayer {
	return PurgeLayer{Name: name, State: PurgeSkipped, Detail: reason}
}

// PurgeDomain clears the server-side caches the panel runs for one domain: its Varnish objects, its scripts in OPcache and the object cache of WordPress sites on it. The caller checks ownership first.
func PurgeDomain(ctx context.Context, a *appctx.App, userContext, domain string) (PurgeResult, error) {
	res := PurgeResult{Domain: domain}
	if !purgeDomainRE.MatchString(domain) {
		return res, errors.New("invalid domain")
	}

	var docroot string
	if err := a.DB.QueryRowContext(ctx, "SELECT docroot FROM domains WHERE domain_url = ?", domain).Scan(&docroot); err != nil {
		return res, errors.New("unable to detect docroot for the domain")
	}
	docroot = filepath.Clean(docroot)
	if !strings.HasPrefix(docroot, "/var/www/html/") {
		docroot = ""
	}

	ws := strings.TrimSpace(webserver.GetEnvFileValue(userContext, "WEB_SERVER"))
	f := purgeFacts{Docroot: docroot, Litespeed: strings.Contains(strings.ToLower(ws), "litespeed")}
	phpVersion := php.GetPHPVForDomain(ctx, a, userContext, domain)
	switch {
	case f.Litespeed:
		f.PHPContainer = ws
	case phpVersion != "" && phpVersion != "/":
		f.PHPContainer = "php-fpm-" + phpVersion
	}
	if !purgeContainerRE.MatchString(f.PHPContainer) || (!f.Litespeed && !docker.IsServiceRunning(ctx, userContext, f.PHPContainer)) {
		f.PHPContainer = ""
	}
	f.VarnishRunning = docker.IsServiceRunning(ctx, userContext, "varnish")
	if f.VarnishRunning {
		f.VarnishDomain = DomainVarnishStatus(domain)
	}
	f.WPSites = wordpressSitesOn(ctx, a, domain)

	skipVarnish, skipOpcache, skipObject := purgePlan(f)

	if skipVarnish != "" {
		res.Layers = append(res.Layers, skipped("varnish", skipVarnish))
	} else if err := BanVarnish(ctx, userContext, domain, ""); err != nil {
		res.Layers = append(res.Layers, PurgeLayer{Name: "varnish", State: PurgeFailed, Detail: err.Error()})
	} else {
		res.Layers = append(res.Layers, PurgeLayer{Name: "varnish", State: PurgeDone})
	}

	if skipOpcache != "" {
		res.Layers = append(res.Layers, skipped("opcache", skipOpcache))
	} else {
		res.Layers = append(res.Layers, PurgeOpcache(ctx, userContext, f.PHPContainer, f.Docroot))
	}

	if skipObject != "" {
		res.Layers = append(res.Layers, skipped("object_cache", skipObject))
	} else {
		for _, site := range f.WPSites {
			siteRoot := f.Docroot
			if _, folder, ok := strings.Cut(site, "/"); ok {
				siteRoot = f.Docroot + "/" + folder
			}
			FlushWPObjectCache(ctx, userContext, f.PHPContainer, site, siteRoot)
		}
		res.Layers = append(res.Layers, PurgeLayer{Name: "object_cache", State: PurgeDone, Count: len(f.WPSites)})
	}
	return res, nil
}

// wordpressSitesOn lists the WordPress installs on domain and in folders under it, by site_name
func wordpressSitesOn(ctx context.Context, a *appctx.App, domain string) []string {
	rows, err := a.DB.QueryContext(ctx, "SELECT site_name FROM sites WHERE LOWER(type) = 'wordpress' AND (site_name = ? OR site_name LIKE ?)", domain, domain+"/%")
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if rows.Scan(&name) != nil {
			continue
		}
		if _, folder, ok := strings.Cut(name, "/"); ok && (!purgeFolderRE.MatchString(folder) || strings.Contains(folder, "..")) {
			continue
		}
		out = append(out, name)
	}
	return out
}

func podmanOutput(ctx context.Context, userContext string, timeout time.Duration, args ...string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := podmanmanager.Command(runCtx, userContext, podmanmanager.PodmanArgv(userContext, args...)).CombinedOutput()
	return string(out), err
}

// BanVarnish drops a site's cached objects, using the x-host/x-url headers the VCL stores on every object. An empty folder bans the whole domain.
func BanVarnish(ctx context.Context, userContext, domain, folder string) error {
	for _, expr := range varnishBanExprs(domain, folder) {
		args := append([]string{"exec", "varnish", "varnishadm", "ban"}, expr...)
		if out, err := podmanOutput(ctx, userContext, 15*time.Second, args...); err != nil {
			return fmt.Errorf("%v: %s", err, strings.TrimSpace(out))
		}
	}
	return nil
}

func varnishBanExprs(domain, folder string) [][]string {
	var out [][]string
	for _, host := range []string{domain, "www." + domain} {
		expr := []string{"obj.http.x-host", "==", host}
		if folder != "" {
			expr = append(expr, "&&", "obj.http.x-url", "~", "^/"+regexp.QuoteMeta(folder)+"(/|$)")
		}
		out = append(out, expr)
	}
	return out
}

// WPRedisPrefix keeps each WordPress site's keys apart inside the account's shared redis
func WPRedisPrefix(site string) string {
	return "op_" + redisPrefixRE.ReplaceAllString(strings.ToLower(site), "_") + ":"
}

// FlushRedisPrefix deletes the keys under prefix in the account's redis, other sites' keys stay
func FlushRedisPrefix(ctx context.Context, userContext, prefix string) {
	if !docker.IsServiceRunning(ctx, userContext, "redis") {
		return
	}
	_, _ = podmanOutput(ctx, userContext, 30*time.Second, "exec", "redis", "sh", "-c",
		`redis-cli --scan --pattern "$1*" | xargs -r -n 500 redis-cli del >/dev/null`, "sh", prefix)
}

// FlushWPObjectCache clears one WordPress site's object cache: its redis keys, or whatever another drop-in knows how to flush
func FlushWPObjectCache(ctx context.Context, userContext, phpContainer, site, docroot string) {
	FlushRedisPrefix(ctx, userContext, WPRedisPrefix(site))
	hostPath := "/home/" + userContext + "/docker-data/volumes/" + userContext + "_html_data/_data/" + strings.TrimPrefix(docroot, "/var/www/html/")
	dropin, err := tenantfs.ReadFile(filepath.Join(hostPath, "wp-content", "object-cache.php"))
	if err != nil || strings.Contains(string(dropin), RedisDropinMarker) {
		return
	}
	// another object cache plugin (memcached etc.) only knows how to flush itself
	argv := podmanmanager.BuildWPCLIBaseCommand(userContext, phpContainer)
	argv = append(argv, "--path="+docroot, "--allow-root", "cache", "flush", "--skip-themes")
	runCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	_, _ = podmanmanager.Command(runCtx, userContext, argv).CombinedOutput()
}

// ReloadPHPFPM sends the fpm master in container a graceful reload, which also empties its opcache
func ReloadPHPFPM(ctx context.Context, userContext, container string) {
	_, _ = podmanOutput(ctx, userContext, 15*time.Second, "exec", container, "sh", "-c",
		`for p in /proc/[0-9]*; do grep -q "php-fpm: master" "$p/cmdline" 2>/dev/null && kill -USR2 "${p#/proc/}"; done; true`)
}

// fpmReloadGap is how soon the same fpm master may be reloaded again, a bulk purge would otherwise restart it once per domain
const fpmReloadGap = 10 * time.Second

var (
	fpmReloadMu   sync.Mutex
	fpmReloadLast = map[string]time.Time{}
)

// claimFPMReload reports whether key was last reloaded more than fpmReloadGap before now, and records now when so
func claimFPMReload(key string, now time.Time) bool {
	fpmReloadMu.Lock()
	defer fpmReloadMu.Unlock()
	if last, ok := fpmReloadLast[key]; ok && now.Sub(last) < fpmReloadGap {
		return false
	}
	fpmReloadLast[key] = now
	return true
}

// PurgeOpcache drops docroot's scripts from the pool's opcache and falls back to a graceful fpm reload when the opcache functions are unavailable
func PurgeOpcache(ctx context.Context, userContext, container, docroot string) PurgeLayer {
	layer := PurgeLayer{Name: "opcache"}
	out, err := podmanOutput(ctx, userContext, 30*time.Second, "exec", container,
		"php", "-d", "disable_functions=", "-d", "open_basedir=none", "-d", "display_errors=0", "-r", opcacheInvalidatePHP, "--", strings.TrimRight(docroot, "/")+"/")
	var probe struct {
		Enabled bool `json:"enabled"`
		OK      bool `json:"ok"`
		Count   int  `json:"count"`
	}
	if err == nil && json.Unmarshal([]byte(strings.TrimSpace(out)), &probe) == nil {
		if !probe.Enabled {
			layer.State, layer.Detail = PurgeSkipped, "OPcache is off"
			return layer
		}
		if probe.OK {
			if probe.Count == 0 {
				layer.State, layer.Detail = PurgeSkipped, "No PHP scripts of this domain are cached"
				return layer
			}
			layer.State, layer.Count = PurgeDone, probe.Count
			return layer
		}
	}
	// the pool can't list or invalidate its scripts (disabled functions, restrict_api) or didn't answer
	if claimFPMReload(userContext+"/"+container, time.Now()) {
		ReloadPHPFPM(ctx, userContext, container)
	}
	layer.State, layer.Shared = PurgeDone, true
	return layer
}

// opcacheInvalidatePHP asks php-fpm over FastCGI to drop the scripts under argv[1] from its opcache, the CLI has its own separate one
const opcacheInvalidatePHP = `
$root = $argv[1];
$probe = '/tmp/.openpanel_opcache_purge_' . getmypid() . '.php';
file_put_contents($probe, '<?php $root = ' . var_export($root, true) . '; $on = (bool) ini_get("opcache.enable"); $s = (function_exists("opcache_get_status") && function_exists("opcache_invalidate")) ? @opcache_get_status(true) : false; $n = 0; if (is_array($s)) { foreach (array_keys(isset($s["scripts"]) ? $s["scripts"] : []) as $f) { if (strncmp($f, $root, strlen($root)) === 0 && @opcache_invalidate($f, true)) { $n++; } } } echo json_encode(["enabled" => $on, "ok" => is_array($s), "count" => $n]);');
$fp = @fsockopen('127.0.0.1', 9000, $en, $es, 3);
// socket mode (opencli php-listen socket): php-fpm listens only on /run/php/php-fpm-X.Y/www.sock
if (!$fp) { foreach (glob('/run/php/php-fpm-*/www.sock') ?: [] as $sock) { $fp = @fsockopen('unix://' . $sock, -1, $en, $es, 3); if ($fp) break; } }
if (!$fp) { @unlink($probe); fwrite(STDERR, "php-fpm is not reachable: $es"); exit(1); }
stream_set_timeout($fp, 20);
$rec = function ($t, $c) { $l = strlen($c); return chr(1) . chr($t) . "\x00\x01" . chr($l >> 8) . chr($l & 255) . "\x00\x00" . $c; };
$nv = function ($n, $v) { $a = strlen($n); $b = strlen($v); return ($a < 128 ? chr($a) : pack('N', $a | 0x80000000)) . ($b < 128 ? chr($b) : pack('N', $b | 0x80000000)) . $n . $v; };
$params = ['SCRIPT_FILENAME' => $probe, 'SCRIPT_NAME' => '/probe.php', 'REQUEST_METHOD' => 'GET', 'QUERY_STRING' => '', 'SERVER_PROTOCOL' => 'HTTP/1.1', 'GATEWAY_INTERFACE' => 'CGI/1.1', 'REMOTE_ADDR' => '127.0.0.1', 'SERVER_NAME' => 'localhost', 'CONTENT_LENGTH' => '0'];
$p = ''; foreach ($params as $k => $v) { $p .= $nv($k, $v); }
fwrite($fp, $rec(1, "\x00\x01\x00\x00\x00\x00\x00\x00") . $rec(4, $p) . $rec(4, '') . $rec(5, ''));
$read = function ($n) use ($fp) { $d = ''; while (strlen($d) < $n && !feof($fp)) { $c = fread($fp, $n - strlen($d)); if ($c === false || $c === '') break; $d .= $c; } return $d; };
$out = '';
while (!feof($fp)) {
    $h = $read(8); if (strlen($h) < 8) break;
    $u = unpack('Cver/Ctype/nid/nlen/Cpad/Cres', $h);
    $c = $u['len'] ? $read($u['len']) : ''; if ($u['pad']) $read($u['pad']);
    if ($u['type'] == 6) $out .= $c;
    if ($u['type'] == 3) break;
}
fclose($fp); @unlink($probe);
$pos = strpos($out, "\r\n\r\n");
echo $pos === false ? $out : substr($out, $pos + 4);
`
