package ui

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ypsilonmeister/quota-fleet/internal/agentver"
)

// dictionaryFile is the only UI source file that may contain Japanese text (docs/17).
const dictionaryFile = "assets/i18n.js"

// japaneseText matches kana, kanji, CJK punctuation and full-width forms.
var japaneseText = regexp.MustCompile(`[\x{3000}-\x{30ff}\x{3400}-\x{4dbf}\x{4e00}-\x{9fff}\x{f900}-\x{faff}\x{ff00}-\x{ffef}]`)

// dictionaries parses MESSAGES in i18n.js. It does not need node, and unlike a JavaScript
// engine it sees a key that is defined twice. The format is deliberately strict: one
// `"key": "value",` entry per line.
func dictionaries(t *testing.T) map[string]map[string]string {
	t.Helper()
	block := regexp.MustCompile(`(?s)\n  const MESSAGES = \{\n(.*?)\n  \};\n`).FindSubmatch(i18nJS)
	if block == nil {
		t.Fatal("i18n.js has no MESSAGES block")
	}
	var (
		open   = regexp.MustCompile(`^    ([a-zA-Z-]+): \{$`)
		entry  = regexp.MustCompile(`^      ("(?:[^"\\]|\\.)*"): ("(?:[^"\\]|\\.)*"),?$`)
		locale string
		result = map[string]map[string]string{}
	)
	for _, line := range strings.Split(string(block[1]), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		if m := open.FindStringSubmatch(line); m != nil && locale == "" {
			locale = m[1]
			if result[locale] != nil {
				t.Fatalf("i18n.js defines locale %q twice", locale)
			}
			result[locale] = map[string]string{}
			continue
		}
		if (line == "    }" || line == "    },") && locale != "" {
			locale = ""
			continue
		}
		m := entry.FindStringSubmatch(line)
		if m == nil || locale == "" {
			t.Fatalf("i18n.js: unexpected line in MESSAGES: %q", line)
		}
		key, keyErr := strconv.Unquote(m[1])
		value, valueErr := strconv.Unquote(m[2])
		if keyErr != nil || valueErr != nil {
			t.Fatalf("i18n.js: cannot read entry %q", line)
		}
		if _, ok := result[locale][key]; ok {
			t.Errorf("i18n.js: %s defines %q twice", locale, key)
		}
		result[locale][key] = value
	}
	return result
}

func assertJapaneseMessages(t *testing.T, want map[string]string) {
	t.Helper()
	ja := dictionaries(t)["ja"]
	for key, text := range want {
		if ja[key] != text {
			t.Errorf("ja message %q = %q, want %q", key, ja[key], text)
		}
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func TestUII18nLocalesHaveSameKeys(t *testing.T) {
	all := dictionaries(t)
	if len(all) != 2 || len(all["ja"]) == 0 || len(all["en"]) == 0 {
		t.Fatalf("i18n.js must define exactly the locales ja and en, got %d", len(all))
	}
	ja, en := all["ja"], all["en"]
	for _, key := range sortedKeys(ja) {
		if _, ok := en[key]; !ok {
			t.Errorf("en is missing %q", key)
		}
	}
	for _, key := range sortedKeys(en) {
		if _, ok := ja[key]; !ok {
			t.Errorf("ja is missing %q", key)
		}
	}
	placeholder := regexp.MustCompile(`\{\w+\}`)
	names := func(s string) []string {
		found := placeholder.FindAllString(s, -1)
		slices.Sort(found)
		return slices.Compact(found)
	}
	for _, key := range sortedKeys(ja) {
		other, ok := en[key]
		if !ok {
			continue
		}
		if ja[key] == "" || other == "" {
			t.Errorf("%q has an empty message", key)
		}
		if a, b := names(ja[key]), names(other); !slices.Equal(a, b) {
			t.Errorf("%q placeholders differ: ja %q, en %q", key, a, b)
		}
	}
	// A language is named in that language; everything else in en is English.
	for _, key := range sortedKeys(en) {
		if key != "lang.jaName" && japaneseText.MatchString(en[key]) {
			t.Errorf("en message %q contains Japanese text: %q", key, en[key])
		}
	}
}

func TestUINoJapaneseOutsideDictionary(t *testing.T) {
	if !japaneseText.Match(i18nJS) {
		t.Fatal("the scan does not detect the Japanese text in the dictionary")
	}
	for _, sample := range []string{"あ", "ア", "漢", "、", "（", "：", "　"} {
		if !japaneseText.MatchString(sample) {
			t.Errorf("the scan does not detect %q", sample)
		}
	}

	// The directory is walked, not the embed list, so that a new file is scanned at once.
	var scanned []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		switch filepath.Ext(path) {
		case ".html", ".js", ".mjs", ".css", ".json", ".webmanifest", ".svg":
		default:
			return nil
		}
		path = filepath.ToSlash(path)
		if path == dictionaryFile {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned = append(scanned, path)
		for i, line := range strings.Split(string(data), "\n") {
			if japaneseText.MatchString(line) {
				t.Errorf("%s:%d has Japanese text outside %s: %s", path, i+1, dictionaryFile, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"index.html", "assets/app.js", "assets/direction.js", "assets/notify.js",
		"assets/sw.js", "assets/style.css", "assets/manifest.webmanifest"} {
		if !slices.Contains(scanned, want) {
			t.Errorf("the scan did not read %s", want)
		}
	}
}

// Keys are written as whole string literals, so references can be checked both ways.
func TestUII18nKeyReferences(t *testing.T) {
	ja := dictionaries(t)["ja"]
	var namespaces []string
	for key := range ja {
		namespace, _, _ := strings.Cut(key, ".")
		namespaces = append(namespaces, regexp.QuoteMeta(namespace))
	}
	slices.Sort(namespaces)
	literal := regexp.MustCompile(`"((?:` + strings.Join(slices.Compact(namespaces), "|") + `)\.[A-Za-z0-9]+)"`)

	used := map[string]bool{}
	for name, src := range map[string][]byte{"app.js": appJS, "notify.js": notifyJS} {
		for _, m := range literal.FindAllSubmatch(src, -1) {
			key := string(m[1])
			used[key] = true
			if _, ok := ja[key]; !ok {
				t.Errorf("%s uses %q, which is not in the dictionary", name, key)
			}
		}
		// A call with a literal outside the namespaces above is a misspelled key.
		for _, m := range regexp.MustCompile(`\b(?:t|L|errorAlert)\(\s*"([^"]*)"`).FindAllSubmatch(src, -1) {
			if _, ok := ja[string(m[1])]; !ok {
				t.Errorf("%s calls a message function with unknown key %q", name, m[1])
			}
		}
	}

	html := string(indexHTML)
	for _, m := range regexp.MustCompile(`\sdata-i18n(-[a-z-]+)?="([^"]*)"`).FindAllStringSubmatch(html, -1) {
		attribute, key := strings.TrimPrefix(m[1], "-"), m[2]
		used[key] = true
		if _, ok := ja[key]; !ok {
			t.Errorf("index.html uses %q, which is not in the dictionary", key)
		}
		if attribute != "" && !strings.Contains(string(i18nJS), `"`+attribute+`"`) {
			t.Errorf("index.html uses data-i18n-%s, which i18n.js does not translate", attribute)
		}
	}
	// data-i18n replaces the content, so the element must not have any of its own.
	empty := regexp.MustCompile(`<[a-z0-9]+\s[^<>]*\bdata-i18n="[^"]*"[^<>]*></[a-z0-9]+>`)
	if got, want := len(empty.FindAllString(html, -1)), strings.Count(html, ` data-i18n="`); got != want || want == 0 {
		t.Errorf("index.html: %d of %d data-i18n elements are empty", got, want)
	}

	for _, key := range sortedKeys(ja) {
		if !used[key] {
			t.Errorf("dictionary key %q is not used", key)
		}
	}
}

func TestUII18nAssetsWiring(t *testing.T) {
	html := string(indexHTML)
	for _, token := range []string{`id="locale-switch"`, `data-set-locale="ja"`, `data-set-locale="en"`} {
		if !strings.Contains(html, token) {
			t.Errorf("index.html missing %q", token)
		}
	}
	// The header buttons store the choice; other tabs follow without writing it back.
	js := string(appJS)
	for _, token := range []string{
		`i18n.setLocale(btn.getAttribute("data-set-locale"), true);`,
		"if (ev && ev.key === i18n.LOCALE_KEY) {\n      i18n.setLocale(ev.newValue, false);",
	} {
		if !strings.Contains(js, token) {
			t.Errorf("app.js missing language switch wiring %q", token)
		}
	}
	// A translated string handed to the DOM would not follow a language change (docs/17).
	eager := regexp.MustCompile(`(?:\b(?:text|placeholder|title)|"aria-label"):\s*t\(|\b(?:navLink|showBanner|setLoading|kv)\([^\n]*\bt\(|\.(?:textContent|title|placeholder)\s*=[^=]`)
	for _, found := range eager.FindAllString(js, -1) {
		t.Errorf("app.js puts a message on the page that is not redrawn: %q (use L, or i18n.setText)", found)
	}
}

// Item names come from the dictionary even when both languages spell them the same.
// The exceptions are inputs labelled with the API field name (docs/17).
func TestUII18nItemNamesFromDictionary(t *testing.T) {
	literal := regexp.MustCompile(`(?:\bkv\(\s*|el\("(?:th|h1|h2)",[^\n]*\btext:\s*|"field-label",\s*text:\s*|\bnavLink\([^,\n]+,\s*)"([^"]*[A-Za-z][^"]*)"`)
	apiNames := []string{"target_ref", "worktree_spec.repo_path", "worktree_spec.base_ref", "worktree_spec.branch"}
	for _, m := range literal.FindAllStringSubmatch(string(appJS), -1) {
		if !slices.Contains(apiNames, m[1]) {
			t.Errorf("app.js writes the item name %q directly (put it in the dictionary): %s", m[1], m[0])
		}
	}
}

// The dictionary is delivered inside app.js. A separate file could fail to load on its
// own and leave a page without any label or working control.
func TestUII18nDictionaryServedWithApp(t *testing.T) {
	rec := get(t, "/ui/assets/app.js")
	if rec.Code != 200 || rec.Body.String() != string(i18nJS)+";\n"+string(appJS) {
		t.Errorf("GET /ui/assets/app.js must serve the dictionary followed by the application (status %d)", rec.Code)
	}
	if rec := get(t, "/ui/assets/i18n.js"); rec.Code != 404 {
		t.Errorf("GET /ui/assets/i18n.js status = %d, want 404", rec.Code)
	}
	if strings.Contains(string(indexHTML), "i18n.js") {
		t.Errorf("index.html must not load the dictionary on its own")
	}
}

// Runs the scripts as the browser receives them: in index.html order, without require.
func TestUII18nServedScriptsWithNode(t *testing.T) {
	var served []string
	for _, m := range regexp.MustCompile(`<script[^>]*\ssrc="([^"]+)"`).FindAllStringSubmatch(string(indexHTML), -1) {
		served = append(served, get(t, m[1]).Body.String())
	}
	input, err := json.Marshal(served)
	if err != nil {
		t.Fatal(err)
	}
	script := `
const assert = require("assert");
const fs = require("fs");
const vm = require("vm");
const [direction, notify, bundle] = JSON.parse(fs.readFileSync(0, "utf8"));

// notify.js runs before the dictionary exists and finds it when it needs a message.
const page = vm.createContext({});
vm.runInContext(direction, page);
vm.runInContext(notify, page);
assert.strictEqual(page.__qfI18n, undefined);
// The bundle runs as one script; without a document the application stops after its helpers,
// which it can only reach by finding the dictionary.
vm.runInContext(bundle, page);
assert.strictEqual(typeof page.__qfI18n.t, "function");
let shown;
page.__qfNotify.createJobNotifier({
  getNotification: () => ({ permission: "granted" }),
  show: message => { shown = [message.title, message.body]; },
  onError: error => { throw error; }
})({ job_id: 3, status: "done", title: "x" });
assert.ok(shown && shown[0].includes("#3") && !shown[1].includes("notify."), String(shown));
console.log("i18n served ok");
`
	runNode(t, "i18n served ok", script, input)
}

func TestUII18nLimitationNoteMatchesServer(t *testing.T) {
	assertJapaneseMessages(t, map[string]string{"agents.limitationNote": agentver.LimitationNote})
}

// runNode runs a script in this directory; it must print marker when it finishes.
func runNode(t *testing.T, marker, script string, stdin []byte) {
	t.Helper()
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available in PATH; skipping JS i18n test")
	}
	cmd := exec.Command(nodeBin, "-e", script)
	cmd.Stdin = strings.NewReader(string(stdin))
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), marker) {
		t.Fatalf("node test failed: %v\n%s", err, out)
	}
}

func TestUII18nRuntimeWithNode(t *testing.T) {
	parsed, err := json.Marshal(dictionaries(t))
	if err != nil {
		t.Fatal(err)
	}
	script := `
const assert = require("assert");
const fs = require("fs");
const vm = require("vm");
const source = fs.readFileSync("assets/i18n.js", "utf8");
const i18n = require("./assets/i18n.js");

// The parser used by the Go tests sees the same dictionaries as JavaScript.
assert.deepStrictEqual(i18n.MESSAGES, JSON.parse(fs.readFileSync(0, "utf8")));

// Initial language: ja for Japanese browsers, otherwise en; a stored choice wins.
for (const [language, want] of [["ja", "ja"], ["ja-JP", "ja"], ["JA_jp", "ja"], ["en-US", "en"],
    ["fr", "en"], ["jam", "en"], ["", "en"], [undefined, "en"]]) {
  assert.strictEqual(i18n.detectLocale(language), want, String(language));
}
assert.strictEqual(i18n.resolveLocale("en", "ja"), "en");
assert.strictEqual(i18n.resolveLocale("ja", "en"), "ja");
assert.strictEqual(i18n.resolveLocale("de", "ja"), "ja");
assert.strictEqual(i18n.resolveLocale("toString", "en"), "en");
assert.strictEqual(i18n.resolveLocale(null, "en"), "en");

// Outside a page the source language is used.
assert.strictEqual(i18n.getLocale(), "ja");
assert.strictEqual(i18n.t("common.jobLabel", { id: 7 }), "ジョブ #7");
i18n.setLocale("en", false);
assert.strictEqual(i18n.t("common.jobLabel", { id: 7 }), "Job #7");
assert.strictEqual(i18n.t("no.such.key"), "no.such.key");
assert.strictEqual(i18n.t("toString"), "toString");
// Values are inserted literally and are not expanded again.
assert.strictEqual(i18n.t("detail.heading", { id: 1, title: "$& {id}" }), "Job #1: $& {id}");
assert.strictEqual(i18n.t("detail.heading", { id: 0 }), "Job #0: {title}");
// A key missing from a locale falls back to the source text.
const saved = i18n.MESSAGES.en["common.all"];
delete i18n.MESSAGES.en["common.all"];
assert.strictEqual(i18n.t("common.all"), "すべて");
i18n.MESSAGES.en["common.all"] = saved;

// In a page: boot, switching, storage and in-place translation.
function element(attrs, children) {
  const node = {
    attrs: Object.assign({}, attrs),
    childNodes: children || [],
    getAttribute(name) { return name in this.attrs ? this.attrs[name] : null; },
    setAttribute(name, value) { this.attrs[name] = String(value); },
    classes: new Set(),
    get textContent() { return this.childNodes.map(c => c.textContent).join(""); },
    set textContent(value) { this.childNodes = value === "" ? [] : [{ textContent: String(value) }]; }
  };
  node.classList = { toggle: (name, on) => { on ? node.classes.add(name) : node.classes.delete(name); } };
  return node;
}
function boot(options) {
  const store = new Map(options.stored ? [["qf-locale", options.stored]] : []);
  const label = element({ "data-i18n": "nav.jobs" });
  const input = element({ "data-i18n-placeholder": "auth.tokenPlaceholder", "data-i18n-aria-label": "auth.tokenLabel", "data-i18n-title": "header.token" });
  const buttons = ["ja", "en"].map(l => element({ "data-set-locale": l }));
  const switcher = element({}, buttons);
  switcher.querySelectorAll = () => buttons;
  const html = element({ lang: "ja" }, [label, input, switcher]);
  const sandbox = {
    window: {},
    navigator: { language: options.language },
    document: { documentElement: html, getElementById: id => id === "locale-switch" ? switcher : null },
    localStorage: {
      getItem: k => { if (options.broken) throw new Error("denied"); return store.has(k) ? store.get(k) : null; },
      setItem: (k, v) => { if (options.broken) throw new Error("denied"); store.set(k, v); }
    }
  };
  vm.runInNewContext(source, sandbox);
  return { api: sandbox.__qfI18n, store, html, label, input, buttons };
}
{
  const page = boot({ language: "en-GB" });
  assert.strictEqual(page.api.getLocale(), "en");
  assert.strictEqual(page.html.getAttribute("lang"), "en");
  assert.strictEqual(page.label.textContent, "Jobs");
  assert.strictEqual(page.input.getAttribute("placeholder"), "api.token in fleet.yaml");
  assert.strictEqual(page.input.getAttribute("aria-label"), "Bearer token");
  assert.strictEqual(page.input.getAttribute("title"), "Token");
  assert.deepStrictEqual(page.buttons.map(b => b.getAttribute("aria-pressed")), ["false", "true"]);
  assert.strictEqual(page.store.size, 0, "a detected language is not stored");

  // Bound messages follow the switch; a plain string removes the binding.
  const bound = element(), plain = element(), text = { textContent: "" };
  let calls = 0;
  page.api.setText(bound, () => { calls++; return page.api.t("common.loading"); });
  page.api.setAttr(bound, "title", page.api.lazy("common.all"));
  page.api.setText(text, page.api.lazy("common.clear"));
  page.api.setText(plain, page.api.lazy("common.loading"));
  page.api.setText(plain, "from the server");
  page.api.setAttr(plain, "title", page.api.lazy("common.all"));
  page.api.setAttr(plain, "title", "fixed");
  // A binding wins over the static attribute of the same node.
  page.api.setAttr(page.input, "title", page.api.lazy("common.filter"));
  page.html.childNodes.push(bound, plain, text);
  assert.strictEqual(bound.textContent, "Loading...");

  page.api.setLocale("ja", true);
  assert.strictEqual(page.store.get("qf-locale"), "ja");
  assert.strictEqual(page.html.getAttribute("lang"), "ja");
  assert.strictEqual(page.label.textContent, "ジョブ一覧");
  assert.strictEqual(bound.textContent, "読み込み中...");
  assert.strictEqual(bound.getAttribute("title"), "すべて");
  assert.strictEqual(text.textContent, "クリア");
  assert.strictEqual(plain.textContent, "from the server");
  assert.strictEqual(plain.getAttribute("title"), "fixed");
  assert.strictEqual(page.input.getAttribute("title"), "絞り込み");
  assert.deepStrictEqual(page.buttons.map(b => b.getAttribute("aria-pressed")), ["true", "false"]);
  assert.deepStrictEqual(page.buttons.map(b => b.classes.has("active")), [true, false]);
  assert.strictEqual(calls, 2);

  // Following another tab: not written back; a removed choice means detection again.
  page.store.clear();
  page.api.setLocale("en", false);
  assert.strictEqual(page.store.size, 0);
  page.api.setLocale(null, false);
  assert.strictEqual(page.api.getLocale(), "en");
}
assert.strictEqual(boot({ language: "ja-JP" }).api.getLocale(), "ja");
assert.strictEqual(boot({ language: "ja-JP", stored: "en" }).api.getLocale(), "en", "the choice survives a reload");
assert.strictEqual(boot({ language: "en", stored: "ja" }).label.textContent, "ジョブ一覧");
assert.strictEqual(boot({ language: "en", stored: "xx" }).api.getLocale(), "en");
{
  const page = boot({ language: "ja", broken: true });
  assert.strictEqual(page.api.getLocale(), "ja");
  page.api.setLocale("en", true);
  assert.strictEqual(page.label.textContent, "Jobs", "switching works without storage");
}
console.log("ALL_UI_I18N_TESTS_PASSED");
`
	runNode(t, "ALL_UI_I18N_TESTS_PASSED", script, parsed)
}

func TestUII18nEnglishMessagesWithNode(t *testing.T) {
	script := `
const assert = require("assert");
const i18n = require("./assets/i18n.js");
const ui = require("./assets/app.js");
const n = require("./assets/notify.js");

const meta = {
  fetchErrors: [{ parentId: 1, status: "waiting", httpStatus: 0, message: "", messageKey: "chain.fetchNetworkError" },
                { parentId: null, status: "", httpStatus: 200, message: "", messageKey: "chain.fetchInvalidResponse" }],
  currentJobChildrenErrors: [{ status: "queued", httpStatus: 500, message: "db" }],
  truncated: true, expandTruncated: true, unexpandedCount: 3, upTruncated: true, pagesTruncated: true
};
const clean = { truncated: true, upTruncated: true };
const messages = () => [
  ui.formatChainProgressSummary({ total: 0 }),
  ui.formatChainProgressSummary({ total: 3, counts: { done: 2, claimed: 1 } }),
  ui.formatChainFetchErrorNotice(meta),
  ui.formatDirectChildrenFetchErrorNotice(meta, 5),
  ui.formatChainTruncationNotice(meta),
  ui.formatChainTruncationNotice(clean),
  ui.formatNoMatchingHostNotice("claude", []),
  ui.formatDerivedPoolLabel("claude"),
  ui.formatDerivedPoolLabel("")
];

const japanese = messages();
assert.strictEqual(japanese[2], "一部の子ジョブ取得に失敗したため、チェーン構成に欠落がある可能性があります（失敗: #1 [waiting: 通信エラー]、不明な親 [不明な状態: HTTP 200 不正なレスポンス形式]）。取得できた分のみ表示しています。");
assert.strictEqual(japanese[3], "ジョブ #5 の直下の子ジョブ取得が一部失敗したため、一覧に欠落がある可能性があります（失敗状態: queued [HTTP 500 db]）。取得できた分のみ表示しています。");
assert.strictEqual(japanese[4], "チェーンが大きいため一部省略されています（他 3 件のノード配下の探索を上限で打ち切りました・祖先の探索を上限で打ち切りました・一部の子ジョブ一覧がページ取得上限に達しました）。");
assert.strictEqual(japanese[5], "チェーンが大きいため一部省略されています（祖先の探索を上限で打ち切りました）。表示中ジョブの直下の子ジョブは全件取得済みです。");

i18n.setLocale("en", false);
const english = messages();
for (const text of english) {
  assert.ok(!/[　-ヿ㐀-鿿＀-￯]/.test(text), text);
  assert.ok(!/\{\w+\}/.test(text), text);
}
assert.strictEqual(english[1], "Total: 3 — done: 2, running: 1");
assert.strictEqual(english[5], "Parts of this large chain are omitted (the ancestor search stopped at its limit). All direct child jobs of the current job were fetched.");
assert.strictEqual(english[7], "pool: claude (auto)");

(async () => {
  // Notifications and toggle messages use the language selected at the time.
  const shown = [];
  const Granted = { permission: "granted" };
  const notify = n.createJobNotifier({ getNotification: () => Granted, show: async m => { shown.push(m); } });
  await notify({ job_id: 4, status: "failed", prev_status: "claimed", title: "" });
  i18n.setLocale("ja", false);
  await notify({ job_id: 5, status: "done", prev_status: "claimed", title: "Build" });
  assert.deepStrictEqual(shown.map(m => [m.title, m.body]), [
    ["Job #4 failed", "(untitled)\nStatus: failed"],
    ["ジョブ #5 が完了しました", "Build\n状態: done（完了）"]
  ]);

  const views = [], told = [];
  let handlers;
  const controller = n.createNotifyController({
    storage: { getItem: () => "on", setItem() {} },
    getNotification: () => Granted,
    isSecureContext: () => true,
    hasCredentials: () => true,
    hasToken: () => true,
    createStream: h => { handlers = h; return { start() {}, stop() {} }; },
    notify() {}, onListChanged() {},
    onChange: v => views.push(v),
    onMessage: (text, key) => told.push([text, key]),
    onUnauthorized() {}
  });
  i18n.setLocale("en", false);
  controller.sync();
  handlers.onFatal(404);
  const view = views[views.length - 1];
  assert.strictEqual(view.problemKey, "notify.fatalDisabled");
  assert.strictEqual(view.problem, "Job notifications are disabled on the server (api.job_events in fleet.yaml).");
  assert.deepStrictEqual(told, [[view.problem, "notify.fatalDisabled"]]);
  console.log("ALL_UI_I18N_MESSAGE_TESTS_PASSED");
})().catch(err => { console.error(err); process.exit(1); });
`
	runNode(t, "ALL_UI_I18N_MESSAGE_TESTS_PASSED", script, nil)
}

func TestCooldownCountdownTextWithNode(t *testing.T) {
	script := `
const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");
const i18n = require("./assets/i18n.js");
const source = fs.readFileSync("assets/app.js", "utf8");
const callback = source.match(/text: (function \(\) \{\s*return [Lt]\("cooldown\.until",[\s\S]*?\n\s*\}) \}\)\);/);
assert.ok(callback, "cooldown countdown callback");
const now = Date.parse("2026-01-01T12:00:00Z");
const render = vm.runInNewContext("(" + callback[1] + ")", {
  t: i18n.t, L: i18n.lazy,
  formatTime: () => "12:01",
  Date: { parse: Date.parse, now: () => now },
  cd: { until: "2026-01-01T12:01:00Z" }
});
const node = { set textContent(value) { this.text = String(value); } };
i18n.setText(node, render);
assert.equal(node.text, i18n.t("cooldown.until", { until: "12:01", minutes: 1 }));
console.log("cooldown countdown ok");
`
	runNode(t, "cooldown countdown ok", script, nil)
}
