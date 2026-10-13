package email

import (
	"context"
	"strings"
	"testing"

	"github.com/dperedo/bosun/internal/integration"
)

func TestTestCredential(t *testing.T) {
	en := newEnv(t, envOpt{})
	ctx := context.Background()
	cred := func(host, user, pw string, extra ...string) integration.Credential {
		f := map[string]string{"host": host, "user": user}
		for i := 0; i+1 < len(extra); i += 2 {
			f[extra[i]] = extra[i+1]
		}
		return integration.Credential{Kind: KindPassword, Secret: pw, Fields: f}
	}

	// OK: IMAP login, then SMTP AUTH on 465; nothing is sent.
	if err := en.e.TestCredential(ctx, cred("imap.example.test", aliceAdr, "pw-alice")); err != nil {
		t.Fatalf("good credential: %v", err)
	}
	if n := len(en.smtp465.messages()); n != 0 {
		t.Errorf("test sent %d messages", n)
	}
	// STARTTLS on 587.
	if err := en.e.TestCredential(ctx, cred("imap.example.test", aliceAdr, "pw-alice", "smtp_port", "587")); err != nil {
		t.Fatalf("587: %v", err)
	}

	// Wrong password: clear message, generic app-password hint, no secret.
	err := en.e.TestCredential(ctx, cred("imap.example.test", aliceAdr, "nope-secret"))
	if err == nil || !strings.Contains(err.Error(), "wrong password") || !strings.Contains(err.Error(), "app password") ||
		strings.Contains(err.Error(), "nope-secret") {
		t.Errorf("bad password: %v", err)
	}

	// Gmail: the hint mentions 2-Step Verification.
	en.imap.setPassword("dave@gmail.com", "right")
	err = en.e.TestCredential(ctx, cred("imap.gmail.com", "dave@gmail.com", "wrong"))
	if err == nil || !strings.Contains(err.Error(), "2-Step Verification") || !strings.Contains(err.Error(), "https://myaccount.google.com/apppasswords") {
		t.Errorf("gmail hint: %v", err)
	}

	// IMAP accepts but SMTP refuses.
	en.imap.setPassword("smtpfail@example.test", "pw")
	err = en.e.TestCredential(ctx, cred("imap.example.test", "smtpfail@example.test", "pw"))
	if err == nil || !strings.Contains(err.Error(), "SMTP server smtp.example.test refused") {
		t.Errorf("smtp refusal: %v", err)
	}

	// Unreachable server.
	err = en.e.TestCredential(ctx, cred("imap.nowhere.test", aliceAdr, "pw-alice"))
	if err == nil || !strings.Contains(err.Error(), "could not reach the IMAP server imap.nowhere.test:993") {
		t.Errorf("unreachable: %v", err)
	}

	// Bad shapes.
	if err := en.e.TestCredential(ctx, cred("", aliceAdr, "x")); err == nil {
		t.Error("missing host accepted")
	}
	if err := en.e.TestCredential(ctx, integration.Credential{Kind: KindGoogleOAuth, Secret: "x"}); err == nil {
		t.Error("oauth kind accepted")
	}
}
