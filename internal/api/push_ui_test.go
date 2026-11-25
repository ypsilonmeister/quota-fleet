package api_test

import (
	"context"
	"encoding/base64"
	"os/exec"
	"strings"
	"testing"
)

// Runs the page's push controller (internal/ui) against this server. A registration
// times out in the page, notifications are turned off while the browser cannot
// unsubscribe, and the server gets the registration only after that (docs/16).
func TestPushControllerLateRegistrationWithNode(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available in PATH; skipping JS web push test")
	}
	script := `
const assert = require("assert");
const crypto = require("crypto");
const n = require("../ui/assets/notify.js");
const [base, mode] = process.argv.slice(1);

const flush = async () => { for (let i = 0; i < 20; i++) await new Promise(r => setImmediate(r)); };
const until = async done => { while (!done()) await new Promise(r => setTimeout(r, 5)); };

async function main() {
  // One browser: its subscription outlives the pages.
  const browser = { current: null, count: 0, broken: false };
  const make = () => {
    const ecdh = crypto.createECDH("prime256v1");
    ecdh.generateKeys();
    const endpoint = "https://push.example/s/" + (++browser.count);
    const keys = { p256dh: ecdh.getPublicKey().toString("base64url"), auth: crypto.randomBytes(16).toString("base64url") };
    return {
      endpoint,
      toJSON: () => ({ endpoint, keys }),
      unsubscribe: async () => {
        if (browser.broken) throw new Error("boom");
        browser.current = null;
        return true;
      }
    };
  };
  const registration = { pushManager: {
    getSubscription: async () => browser.current,
    subscribe: async () => (browser.current = browser.current || make())
  } };
  let enabled = true, hold = false, held = null;
  const answers = [];
  const page = () => {
    const timers = new Map();
    let timer = 0;
    const controller = n.createPushController({
      isSupported: () => true,
      getRegistration: async () => registration,
      findRegistration: async () => registration,
      getToken: () => "tok-subscriber",
      isEnabled: () => enabled,
      setTimeout: fn => { timers.set(++timer, fn); return timer; },
      clearTimeout: id => timers.delete(id),
      onChange: () => {},
      fetch: async (path, init) => {
        const send = async () => {
          const resp = await fetch(base + path, init);
          answers.push(init.method + " " + resp.status);
          return resp;
        };
        if (hold && init.method === "POST") {
          hold = false;
          return new Promise((resolve, reject) => { held = () => send().then(resolve, reject); });
        }
        return send();
      }
    });
    const expire = () => { for (const [id, fn] of timers) { timers.delete(id); fn(); } };
    return { controller, expire };
  };

  let current = page();
  assert.strictEqual((await current.controller.sync()).state, "active");

  hold = true;
  const stuck = current.controller.sync();
  await until(() => held !== null);
  if (mode === "reload") {
    // A new page knows nothing about the registration the old one sent.
    current = page();
  } else {
    current.expire();
    assert.strictEqual((await stuck).state, "error");
  }
  enabled = false;
  browser.broken = true;
  assert.strictEqual((await current.controller.sync()).state, "off");
  answers.length = 0;
  await held();
  await flush();
  assert.deepStrictEqual(answers, ["POST 410"]);

  if (mode === "again") {
    // Turning notifications on replaces the subscription the server no longer takes.
    enabled = true;
    browser.broken = false;
    answers.length = 0;
    assert.strictEqual((await current.controller.sync()).state, "active");
    assert.deepStrictEqual(answers, ["GET 200", "POST 410", "POST 201"]);
    assert.strictEqual(browser.current.endpoint, "https://push.example/s/2");
  }
  console.log("ALL_PUSH_LATE_REGISTRATION_TESTS_PASSED");
}

main().catch(err => { console.error(err); process.exit(1); });
`
	// The page decodes the key before subscribing.
	publicKey := base64.RawURLEncoding.EncodeToString(append([]byte{4}, make([]byte, 64)...))
	for mode, want := range map[string]string{
		"timeout": "",
		"reload":  "",
		"again":   "https://push.example/s/2",
	} {
		t.Run(mode, func(t *testing.T) {
			ts, st := newPushAPI(t, publicKey, true)
			out, err := exec.Command(nodeBin, "-e", script, ts.URL, mode).CombinedOutput()
			if err != nil || !strings.Contains(string(out), "ALL_PUSH_LATE_REGISTRATION_TESTS_PASSED") {
				t.Errorf("node test failed: %v\n%s", err, out)
			}
			stored, err := st.ListPushSubscriptions(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			var endpoints []string
			for _, sub := range stored {
				endpoints = append(endpoints, sub.Endpoint)
			}
			if got := strings.Join(endpoints, " "); got != want {
				t.Errorf("stored endpoints = %q, want %q", got, want)
			}
		})
	}
}
