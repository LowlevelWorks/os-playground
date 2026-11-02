package auth

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/os-playground-api/internal/database"
)

// runStoreContract is the behaviour both stores must share.
func runStoreContract(t *testing.T, newStore func(t *testing.T) Store) {
	ctx := context.Background()
	// Subjects are unique per run, so a shared database needs no cleanup.
	run := strconv.FormatInt(time.Now().UnixNano(), 36)
	ident := func(sub string) Identity {
		return Identity{Issuer: "https://issuer.test", Subject: sub + "-" + run, Email: "a@example.com", Name: "A"}
	}

	t.Run("LoginCreatesThenReusesTheUser", func(t *testing.T) {
		st := newStore(t)
		u1, tok1, err := st.Login(ctx, ident("reuse"), time.Hour, "req-1")
		if err != nil {
			t.Fatal(err)
		}
		id2 := ident("reuse")
		id2.Name, id2.Email = "B", ""
		u2, tok2, err := st.Login(ctx, id2, time.Hour, "")
		if err != nil {
			t.Fatal(err)
		}
		if u1.ID == "" || u1.ID != u2.ID || u2.Name != "B" || u2.Email != "" {
			t.Errorf("users %+v then %+v, want one user with the new details", u1, u2)
		}
		if tok1 == tok2 || len(tok1) != tokenLen {
			t.Errorf("tokens %q and %q, want two distinct %d-character tokens", tok1, tok2, tokenLen)
		}
		for _, tok := range []string{tok1, tok2} { // both sessions stay valid
			if got, err := st.Lookup(ctx, tok); err != nil || got.ID != u1.ID || got.Name != "B" {
				t.Errorf("Lookup = %+v, %v", got, err)
			}
		}
		if other, _, _ := st.Login(ctx, ident("someone-else"), time.Hour, ""); other.ID == u1.ID {
			t.Error("two subjects share a user")
		}
	})

	t.Run("LookupRefusesBadTokens", func(t *testing.T) {
		st := newStore(t)
		for _, tok := range []string{"", "short", newToken(), newToken()[:tokenLen-1] + "!"} {
			if _, err := st.Lookup(ctx, tok); !errors.Is(err, ErrNoSession) {
				t.Errorf("Lookup(%q) = %v, want ErrNoSession", tok, err)
			}
		}
	})

	t.Run("SessionsExpire", func(t *testing.T) {
		st := newStore(t)
		_, tok, err := st.Login(ctx, ident("expire"), 10*time.Millisecond, "")
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond) // the store's own clock decides
		if _, err := st.Lookup(ctx, tok); !errors.Is(err, ErrNoSession) {
			t.Fatalf("Lookup after expiry = %v, want ErrNoSession", err)
		}
		if n, err := st.PurgeExpired(ctx, 100_000); err != nil || n < 1 {
			t.Errorf("PurgeExpired = %d, %v; want at least this session", n, err)
		}
	})

	t.Run("LogoutEndsOnlyThatSession", func(t *testing.T) {
		st := newStore(t)
		_, a, _ := st.Login(ctx, ident("logout"), time.Hour, "")
		_, b, _ := st.Login(ctx, ident("logout"), time.Hour, "")
		if err := st.Logout(ctx, a, "req-2"); err != nil {
			t.Fatal(err)
		}
		if err := st.Logout(ctx, a, ""); !errors.Is(err, ErrNoSession) {
			t.Errorf("second Logout = %v, want ErrNoSession", err)
		}
		if _, err := st.Lookup(ctx, a); !errors.Is(err, ErrNoSession) {
			t.Error("the ended session still works")
		}
		if _, err := st.Lookup(ctx, b); err != nil {
			t.Errorf("the other session ended too: %v", err)
		}
	})

	t.Run("InvalidInput", func(t *testing.T) {
		st := newStore(t)
		for _, tc := range []struct {
			id  Identity
			ttl time.Duration
		}{{Identity{Subject: "s"}, time.Hour}, {Identity{Issuer: "i"}, time.Hour}, {ident("x"), 0}} {
			if _, _, err := st.Login(ctx, tc.id, tc.ttl, ""); !errors.Is(err, ErrInvalid) {
				t.Errorf("Login(%+v, %v) = %v, want ErrInvalid", tc.id, tc.ttl, err)
			}
		}
		if _, err := st.PurgeExpired(ctx, 0); !errors.Is(err, ErrInvalid) {
			t.Errorf("PurgeExpired(0) = %v, want ErrInvalid", err)
		}
	})
}

func TestMemStoreContract(t *testing.T) {
	runStoreContract(t, func(*testing.T) Store { return NewMemStore(time.Now) })
}

func TestPGStoreContract(t *testing.T) {
	url := os.Getenv("PLAYGROUND_TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("PLAYGROUND_TEST_DATABASE_URL is not set in CI")
		}
		t.Skip("PLAYGROUND_TEST_DATABASE_URL not set")
	}
	pool, err := database.New(context.Background(), url, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	store, err := NewPGStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	runStoreContract(t, func(*testing.T) Store { return store })

	// A disabled user cannot sign in, and their open sessions stop working.
	ctx := context.Background()
	id := Identity{Issuer: "https://issuer.test", Subject: "disabled-" + strconv.FormatInt(time.Now().UnixNano(), 36)}
	u, tok, err := store.Login(ctx, id, time.Hour, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE users SET disabled_at = now() WHERE id = $1", u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Lookup(ctx, tok); !errors.Is(err, ErrNoSession) {
		t.Errorf("Lookup for a disabled user = %v, want ErrNoSession", err)
	}
	if _, _, err := store.Login(ctx, id, time.Hour, ""); !errors.Is(err, ErrDisabled) {
		t.Errorf("Login for a disabled user = %v, want ErrDisabled", err)
	}

	// Login and logout are audited by user id only.
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events
		WHERE user_id = $1 AND event_type = $2`, u.ID, EventLoggedIn).Scan(&n); err != nil || n != 1 {
		t.Errorf("login audit rows = %d, %v; want 1", n, err)
	}
	// Only the token's hash is stored.
	var stored int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM login_sessions WHERE token_hash = $1`, []byte(tok)).Scan(&stored); err != nil || stored != 0 {
		t.Errorf("the raw token is stored: %d rows, %v", stored, err)
	}
}
