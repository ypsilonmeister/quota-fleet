package adapter

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
)

func TestSSHTransportBuildArgs(t *testing.T) {
	tr := NewSSHTransport(SSHTransportOptions{
		Timeout:        20 * time.Second,
		ConnectTimeout: 7 * time.Second,
	})

	proxy := "nc -X connect -x 127.0.0.1:8080 %h %p"
	host := config.HostConfig{
		Transport:             "ssh",
		SSH:                   "worker@ubuntu.example.ts.net",
		OS:                    "linux",
		KeyFile:               "/etc/keys/fleet_ed25519",
		ProxyCommand:          &proxy,
		KnownHostsFile:        "/etc/keys/known_hosts",
		StrictHostKeyChecking: "yes",
	}

	args, err := tr.BuildArgs(host, "echo hello")
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}

	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-o BatchMode=yes",
		"-o ConnectTimeout=7",
		"-i /etc/keys/fleet_ed25519",
		"-o IdentitiesOnly=yes",
		"-o ProxyCommand=nc -X connect -x 127.0.0.1:8080 %h %p",
		"-o UserKnownHostsFile=/etc/keys/known_hosts",
		"-o StrictHostKeyChecking=yes",
		"-- worker@ubuntu.example.ts.net echo hello",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("BuildArgs missing %q in %q", want, joined)
		}
	}

	// Empty SSH target must return an error.
	if _, err := tr.BuildArgs(config.HostConfig{Transport: "ssh"}, "echo hi"); err == nil {
		t.Errorf("expected error for empty ssh target")
	}
}

func TestLocalTransportRunAndTimeout(t *testing.T) {
	tr := NewLocalTransport(LocalTransportOptions{
		Timeout: 2 * time.Second,
	})

	// Successful command.
	res, err := tr.Run(context.Background(), config.HostConfig{Transport: "local"}, "printf 'ok'; printf 'warn' >&2")
	if err != nil {
		t.Fatalf("LocalTransport.Run success: %v", err)
	}
	if res.ExitCode != 0 || res.Stdout != "ok" || res.Stderr != "warn" {
		t.Errorf("unexpected result: %+v", res)
	}

	// Non-zero exit code returns nil error and populated ExitCode.
	res, err = tr.Run(context.Background(), config.HostConfig{Transport: "local"}, "echo fail >&2; exit 42")
	if err != nil {
		t.Fatalf("LocalTransport.Run non-zero err = %v, want nil", err)
	}
	if res.ExitCode != 42 || !strings.Contains(res.Stderr, "fail") {
		t.Errorf("unexpected non-zero result: %+v", res)
	}

	// Timeout cancels the command and returns an error.
	shortTr := NewLocalTransport(LocalTransportOptions{
		Timeout: 50 * time.Millisecond,
	})
	if _, err := shortTr.Run(context.Background(), config.HostConfig{Transport: "local"}, "exec sleep 5"); err == nil {
		t.Errorf("expected timeout error from LocalTransport")
	}
}

func TestRoutingTransportDispatchesByHostTransport(t *testing.T) {
	sshFake := NewFakeTransport()
	sshFake.SetDefault(CommandResult{Stdout: "from-ssh"}, nil)
	localFake := NewFakeTransport()
	localFake.SetDefault(CommandResult{Stdout: "from-local"}, nil)

	router := NewRoutingTransport(sshFake, localFake)

	res, err := router.Run(context.Background(), config.HostConfig{Transport: "local"}, "echo 1")
	if err != nil || res.Stdout != "from-local" {
		t.Fatalf("local route = (%+v, %v), want from-local", res, err)
	}

	res, err = router.Run(context.Background(), config.HostConfig{Transport: "ssh", SSH: "u@h"}, "echo 2")
	if err != nil || res.Stdout != "from-ssh" {
		t.Fatalf("ssh route = (%+v, %v), want from-ssh", res, err)
	}

	// Empty transport with ssh target routes to ssh; without ssh target routes to local.
	res, err = router.Run(context.Background(), config.HostConfig{SSH: "u@h"}, "echo 3")
	if err != nil || res.Stdout != "from-ssh" {
		t.Fatalf("implicit ssh route = (%+v, %v), want from-ssh", res, err)
	}
	res, err = router.Run(context.Background(), config.HostConfig{}, "echo 4")
	if err != nil || res.Stdout != "from-local" {
		t.Fatalf("implicit local route = (%+v, %v), want from-local", res, err)
	}

	if _, err := router.Run(context.Background(), config.HostConfig{Transport: "carrier-pigeon"}, "echo 5"); err == nil {
		t.Errorf("expected error for unsupported transport")
	}
}
