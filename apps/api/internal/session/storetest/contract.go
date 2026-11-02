// Package storetest holds the behavioural contract every session.Store must
// pass. The in-memory store runs it with no environment; the PostgreSQL store
// runs exactly the same cases against a real database. If the two disagree,
// the contract decides which one is wrong: never edit a case to suit one.
//
// Every case gets a fresh store from newStore.
package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/os-playground-api/internal/session"
)

const (
	alice = "owner-alice"
	bob   = "owner-bob"
	carol = "owner-carol"

	home   = "203.0.113.1"
	office = "198.51.100.7"
	cafe   = "192.0.2.9"

	// missingID is well formed but never stored.
	missingID = "00000000-0000-7000-8000-000000000000"
)

// roomy limits never refuse; a case tightens the one it tests.
var roomy = session.Limits{Capacity: 100, PerOwner: 100, PerClient: 100}

// Run executes the contract. newStore must return a fresh, empty store.
func Run(t *testing.T, newStore func(t *testing.T) session.Store) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t)
			if st == nil {
				t.Fatal("storetest: newStore returned nil")
			}
			tc.run(t, st)
		})
	}
}

var cases = []struct {
	name string
	run  func(*testing.T, session.Store)
}{
	{"AdmitCreatesASession", admitCreatesASession},
	{"GetIsForTheOwnerOnly", getIsForTheOwnerOnly},
	{"OwnerQuotaNamesTheOldestSession", ownerQuotaNamesTheOldestSession},
	{"OwnerQuotaHoldsOnAnyClient", ownerQuotaHoldsOnAnyClient},
	{"ClientQuotaLeaksNoOtherOwnersID", clientQuotaLeaksNoOtherOwnersID},
	{"CapacityFull", capacityFull},
	{"OwnerQuotaIsCheckedBeforeCapacity", ownerQuotaIsCheckedBeforeCapacity},
	{"RequestDeleteFreesQuotaAtOnce", requestDeleteFreesQuotaAtOnce},
	{"DeletingSessionStillHoldsCapacity", deletingSessionStillHoldsCapacity},
	{"RequestDeleteIsIdempotentAndOwned", requestDeleteIsIdempotentAndOwned},
	{"GetReturnsSessionMarkedForDeletion", getReturnsSessionMarkedForDeletion},
	{"ReturnedSessionIsACopy", returnedSessionIsACopy},
	{"InvalidInputIsRejected", invalidInputIsRejected},
	{"CancelledContext", cancelledContext},
	{"ConcurrentAdmitsRespectOwnerQuota", concurrentAdmitsRespectOwnerQuota},
	{"ConcurrentAdmitsRespectCapacity", concurrentAdmitsRespectCapacity},
	{"MalformedIDIsNotFound", malformedIDIsNotFound},
	{"OtherIDSpellingsFindTheSession", otherIDSpellingsFindTheSession},

	{"ClaimTakesDueSessionsOnce", claimTakesDueSessionsOnce},
	{"ClaimHonoursLimitAndOrder", claimHonoursLimitAndOrder},
	{"ClaimRejectsBadArguments", claimRejectsBadArguments},
	{"CompleteRecordsTheOutcome", completeRecordsTheOutcome},
	{"CompleteNeedsTheLease", completeNeedsTheLease},
	{"ExpiredLeaseCanBeTakenOver", expiredLeaseCanBeTakenOver},
	{"RetryAfterDefersTheSession", retryAfterDefersTheSession},
	{"RequestDeleteMakesTheSessionDue", requestDeleteMakesTheSessionDue},
	{"FailedAttemptsAreCounted", failedAttemptsAreCounted},
	{"ReleasingTheVMFreesCapacity", releasingTheVMFreesCapacity},
	{"DeletedNeedsAReleasedVM", deletedNeedsAReleasedVM},
	{"CompleteRejectsAnUnknownStatus", completeRejectsAnUnknownStatus},
	{"ReconcilerCanDesireDeletion", reconcilerCanDesireDeletion},
	{"TouchActivityPushesTheIdleDeadline", touchActivityPushesTheIdleDeadline},
	{"ConcurrentClaimsNeverShare", concurrentClaimsNeverShare},
}

func newSession(owner, client string) session.NewSession {
	return session.NewSession{
		OwnerID:     owner,
		ClientKey:   client,
		Profile:     "small",
		ImageRef:    "alpine:3.20",
		Spec:        json.RawMessage(`{"cpus": 1, "memory_mib": 512}`),
		MaxDuration: time.Hour,
		IdleTimeout: 15 * time.Minute,
	}
}

func mustAdmit(t *testing.T, st session.Store, owner, client string, l session.Limits) session.Session {
	t.Helper()
	s, err := st.Admit(context.Background(), newSession(owner, client), l)
	if err != nil {
		t.Fatalf("Admit(%s, %s) = %v, want success", owner, client, err)
	}
	return s
}

func quotaError(t *testing.T, err error) *session.QuotaError {
	t.Helper()
	var q *session.QuotaError
	if !errors.As(err, &q) || !errors.Is(err, session.ErrQuota) {
		t.Fatalf("err = %v, want a *QuotaError matching ErrQuota", err)
	}
	return q
}

// sameJSON compares by meaning: jsonb does not keep spacing or key order.
func sameJSON(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("unmarshal %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	return reflect.DeepEqual(x, y)
}

// sameSession compares field by field: times with Equal (locations differ
// between drivers) and Spec as JSON.
func sameSession(t *testing.T, got, want session.Session) {
	t.Helper()
	if !got.CreatedAt.Equal(want.CreatedAt) || !got.ExpiresAt.Equal(want.ExpiresAt) ||
		!got.IdleExpiresAt.Equal(want.IdleExpiresAt) || !sameJSON(t, got.Spec, want.Spec) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	got.CreatedAt, got.ExpiresAt, got.IdleExpiresAt, got.Spec = time.Time{}, time.Time{}, time.Time{}, nil
	want.CreatedAt, want.ExpiresAt, want.IdleExpiresAt, want.Spec = time.Time{}, time.Time{}, time.Time{}, nil
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func admitCreatesASession(t *testing.T, st session.Store) {
	in := newSession(alice, home)
	s, err := st.Admit(context.Background(), in, roomy)
	if err != nil {
		t.Fatalf("Admit = %v", err)
	}
	if s.ID == "" {
		t.Error("ID is empty")
	}
	if s.OwnerID != alice || s.ClientKey != home || s.Profile != in.Profile || s.ImageRef != in.ImageRef {
		t.Errorf("session = %+v, want the input's fields", s)
	}
	if !sameJSON(t, s.Spec, in.Spec) {
		t.Errorf("Spec = %s, want %s", s.Spec, in.Spec)
	}
	if s.Status != session.StatusCreated || s.DesiredDeleted || s.FailureReason != "" {
		t.Errorf("Status = %s, DesiredDeleted = %v, FailureReason = %q; want CREATED, false, empty",
			s.Status, s.DesiredDeleted, s.FailureReason)
	}
	if s.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
	if d := s.ExpiresAt.Sub(s.CreatedAt); d != in.MaxDuration {
		t.Errorf("ExpiresAt - CreatedAt = %v, want %v", d, in.MaxDuration)
	}
	if d := s.IdleExpiresAt.Sub(s.CreatedAt); d != in.IdleTimeout {
		t.Errorf("IdleExpiresAt - CreatedAt = %v, want %v", d, in.IdleTimeout)
	}

	got, err := st.Get(context.Background(), s.ID, alice)
	if err != nil {
		t.Fatalf("Get = %v", err)
	}
	sameSession(t, got, s)

	if other := mustAdmit(t, st, bob, office, roomy); other.ID == s.ID {
		t.Errorf("two sessions share ID %s", s.ID)
	}
}

func getIsForTheOwnerOnly(t *testing.T, st session.Store) {
	s := mustAdmit(t, st, alice, home, roomy)
	ctx := context.Background()
	for _, owner := range []string{bob, ""} {
		if _, err := st.Get(ctx, s.ID, owner); !errors.Is(err, session.ErrNotFound) {
			t.Errorf("Get as %q = %v, want ErrNotFound", owner, err)
		}
	}
	// A well-formed ID that does not exist looks the same as someone else's.
	if _, err := st.Get(ctx, missingID, alice); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("Get(missing) = %v, want ErrNotFound", err)
	}
}

func ownerQuotaNamesTheOldestSession(t *testing.T, st session.Store) {
	l := roomy
	l.PerOwner = 2
	first := mustAdmit(t, st, alice, home, l)
	mustAdmit(t, st, alice, home, l)

	_, err := st.Admit(context.Background(), newSession(alice, home), l)
	q := quotaError(t, err)
	if !q.Yours || q.ID != first.ID || q.ClientWide {
		t.Errorf("QuotaError = %+v, want Yours, ID %s, not ClientWide", q, first.ID)
	}
}

func ownerQuotaHoldsOnAnyClient(t *testing.T, st session.Store) {
	l := roomy
	l.PerOwner = 1
	s := mustAdmit(t, st, alice, home, l)

	_, err := st.Admit(context.Background(), newSession(alice, office), l)
	if q := quotaError(t, err); !q.Yours || q.ID != s.ID {
		t.Errorf("QuotaError = %+v, want Yours with ID %s", q, s.ID)
	}
}

func clientQuotaLeaksNoOtherOwnersID(t *testing.T, st session.Store) {
	l := roomy
	l.PerClient = 1
	mustAdmit(t, st, alice, home, l)

	_, err := st.Admit(context.Background(), newSession(bob, home), l)
	q := quotaError(t, err)
	if q.Yours || q.ID != "" || !q.ClientWide {
		t.Errorf("QuotaError = %+v, want ClientWide only, with no ID", q)
	}

	// Another network is unaffected.
	mustAdmit(t, st, bob, office, l)
}

func capacityFull(t *testing.T, st session.Store) {
	l := roomy
	l.Capacity = 2
	mustAdmit(t, st, alice, home, l)
	mustAdmit(t, st, bob, office, l)

	if _, err := st.Admit(context.Background(), newSession(carol, cafe), l); !errors.Is(err, session.ErrCapacity) {
		t.Errorf("Admit = %v, want ErrCapacity", err)
	}
}

// A double click must offer "Resume it", not a 503, even when the server is
// also full.
func ownerQuotaIsCheckedBeforeCapacity(t *testing.T, st session.Store) {
	l := session.Limits{Capacity: 1, PerOwner: 1, PerClient: 100}
	s := mustAdmit(t, st, alice, home, l)

	_, err := st.Admit(context.Background(), newSession(alice, home), l)
	if q := quotaError(t, err); !q.Yours || q.ID != s.ID {
		t.Errorf("QuotaError = %+v, want Yours with ID %s", q, s.ID)
	}
}

func requestDeleteFreesQuotaAtOnce(t *testing.T, st session.Store) {
	l := session.Limits{Capacity: 100, PerOwner: 1, PerClient: 1}
	s := mustAdmit(t, st, alice, home, l)
	if err := st.RequestDelete(context.Background(), s.ID, alice); err != nil {
		t.Fatalf("RequestDelete = %v", err)
	}
	// Frees both the owner's and the client's quota, before the VM is gone.
	mustAdmit(t, st, alice, home, l)
}

func deletingSessionStillHoldsCapacity(t *testing.T, st session.Store) {
	l := session.Limits{Capacity: 1, PerOwner: 1, PerClient: 1}
	s := mustAdmit(t, st, alice, home, l)
	if err := st.RequestDelete(context.Background(), s.ID, alice); err != nil {
		t.Fatalf("RequestDelete = %v", err)
	}
	// The VM has not been released yet, so the slot is still taken.
	if _, err := st.Admit(context.Background(), newSession(alice, home), l); !errors.Is(err, session.ErrCapacity) {
		t.Errorf("Admit = %v, want ErrCapacity", err)
	}
}

func requestDeleteIsIdempotentAndOwned(t *testing.T, st session.Store) {
	ctx := context.Background()
	s := mustAdmit(t, st, alice, home, roomy)

	for _, owner := range []string{bob, ""} {
		if err := st.RequestDelete(ctx, s.ID, owner); !errors.Is(err, session.ErrNotFound) {
			t.Errorf("RequestDelete as %q = %v, want ErrNotFound", owner, err)
		}
	}
	if got, _ := st.Get(ctx, s.ID, alice); got.DesiredDeleted {
		t.Error("another owner's RequestDelete marked the session")
	}
	if err := st.RequestDelete(ctx, missingID, alice); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("RequestDelete(missing) = %v, want ErrNotFound", err)
	}
	for i := range 2 {
		if err := st.RequestDelete(ctx, s.ID, alice); err != nil {
			t.Errorf("RequestDelete #%d = %v, want nil", i+1, err)
		}
	}
}

func getReturnsSessionMarkedForDeletion(t *testing.T, st session.Store) {
	ctx := context.Background()
	s := mustAdmit(t, st, alice, home, roomy)
	if err := st.RequestDelete(ctx, s.ID, alice); err != nil {
		t.Fatalf("RequestDelete = %v", err)
	}
	got, err := st.Get(ctx, s.ID, alice)
	if err != nil {
		t.Fatalf("Get = %v, want the session: callers decide visibility", err)
	}
	if !got.DesiredDeleted {
		t.Error("DesiredDeleted = false after RequestDelete")
	}
}

func returnedSessionIsACopy(t *testing.T, st session.Store) {
	ctx := context.Background()
	s := mustAdmit(t, st, alice, home, roomy)
	s.Status = session.StatusFailed
	s.Spec[0] = ' '

	got, err := st.Get(ctx, s.ID, alice)
	if err != nil {
		t.Fatalf("Get = %v", err)
	}
	if got.Status != session.StatusCreated || !json.Valid(got.Spec) {
		t.Errorf("changing Admit's result changed the store: %+v", got)
	}
	got.Spec[0] = ' '
	again, _ := st.Get(ctx, s.ID, alice)
	if !json.Valid(again.Spec) {
		t.Error("changing Get's result changed the store")
	}
}

func invalidInputIsRejected(t *testing.T, st session.Store) {
	type mutate = func(*session.NewSession, *session.Limits)
	bad := map[string]mutate{
		"no owner":           func(n *session.NewSession, _ *session.Limits) { n.OwnerID = "" },
		"no client":          func(n *session.NewSession, _ *session.Limits) { n.ClientKey = "" },
		"no profile":         func(n *session.NewSession, _ *session.Limits) { n.Profile = "" },
		"no image":           func(n *session.NewSession, _ *session.Limits) { n.ImageRef = "" },
		"no spec":            func(n *session.NewSession, _ *session.Limits) { n.Spec = nil },
		"bad spec":           func(n *session.NewSession, _ *session.Limits) { n.Spec = json.RawMessage(`{`) },
		"no max duration":    func(n *session.NewSession, _ *session.Limits) { n.MaxDuration = 0 },
		"no idle timeout":    func(n *session.NewSession, _ *session.Limits) { n.IdleTimeout = 0 },
		"zero capacity":      func(_ *session.NewSession, l *session.Limits) { l.Capacity = 0 },
		"zero per owner":     func(_ *session.NewSession, l *session.Limits) { l.PerOwner = 0 },
		"zero per client":    func(_ *session.NewSession, l *session.Limits) { l.PerClient = 0 },
		"negative per owner": func(_ *session.NewSession, l *session.Limits) { l.PerOwner = -1 },
	}
	for name, change := range bad {
		n, l := newSession(alice, home), roomy
		change(&n, &l)
		if _, err := st.Admit(context.Background(), n, l); !errors.Is(err, session.ErrInvalid) {
			t.Errorf("%s: Admit = %v, want ErrInvalid", name, err)
		}
	}
	// Nothing was stored: alice still has her whole quota.
	l := roomy
	l.PerOwner = 1
	mustAdmit(t, st, alice, home, l)
}

func cancelledContext(t *testing.T, st session.Store) {
	s := mustAdmit(t, st, alice, home, roomy)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := st.Admit(ctx, newSession(bob, office), roomy); !errors.Is(err, context.Canceled) {
		t.Errorf("Admit = %v, want context.Canceled", err)
	}
	if _, err := st.Get(ctx, s.ID, alice); !errors.Is(err, context.Canceled) {
		t.Errorf("Get = %v, want context.Canceled", err)
	}
	if err := st.RequestDelete(ctx, s.ID, alice); !errors.Is(err, context.Canceled) {
		t.Errorf("RequestDelete = %v, want context.Canceled", err)
	}
	if got, _ := st.Get(context.Background(), s.ID, alice); got.DesiredDeleted {
		t.Error("RequestDelete with a cancelled context still marked the session")
	}
}

// admitConcurrently runs n Admits at once and returns how many succeeded. Any
// error that want does not accept fails the test.
func admitConcurrently(t *testing.T, st session.Store, n int, l session.Limits,
	input func(i int) session.NewSession, want func(error) bool) int {
	t.Helper()
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		ok    int
		start = make(chan struct{})
	)
	for i := range n {
		wg.Go(func() {
			<-start
			_, err := st.Admit(context.Background(), input(i), l)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case !want(err):
				t.Errorf("Admit #%d = %v", i, err)
			}
		})
	}
	close(start)
	wg.Wait()
	return ok
}

// The race the admission lock exists for: a burst of clicks from one owner.
func concurrentAdmitsRespectOwnerQuota(t *testing.T, st session.Store) {
	l := roomy
	l.PerOwner = 1
	isQuota := func(err error) bool { return errors.Is(err, session.ErrQuota) }
	same := func(int) session.NewSession { return newSession(alice, home) }
	if got := admitConcurrently(t, st, 50, l, same, isQuota); got != 1 {
		t.Errorf("%d Admits succeeded, want exactly 1", got)
	}
}

// Many owners at once must not overfill the server.
func concurrentAdmitsRespectCapacity(t *testing.T, st session.Store) {
	l := roomy
	l.Capacity = 5
	isCapacity := func(err error) bool { return errors.Is(err, session.ErrCapacity) }
	distinct := func(i int) session.NewSession {
		return newSession("owner-"+strconv.Itoa(i), "client-"+strconv.Itoa(i))
	}
	if got := admitConcurrently(t, st, 50, l, distinct, isCapacity); got != l.Capacity {
		t.Errorf("%d Admits succeeded, want exactly %d", got, l.Capacity)
	}
}

// A malformed ID is an ID that does not exist, not a server error. The
// urn:uuid: form parses as a UUID in Go but not in PostgreSQL, so a store
// that passes the raw string to SQL fails here.
func malformedIDIsNotFound(t *testing.T, st session.Store) {
	mustAdmit(t, st, alice, home, roomy)
	ctx := context.Background()
	for _, id := range []string{"", "abc", "not-a-uuid", "'; DROP TABLE playground_sessions; --",
		"00000000-0000-7000-8000-00000000000g", "urn:uuid:" + missingID} {
		if _, err := st.Get(ctx, id, alice); !errors.Is(err, session.ErrNotFound) {
			t.Errorf("Get(%q) = %v, want ErrNotFound", id, err)
		}
		if err := st.RequestDelete(ctx, id, alice); !errors.Is(err, session.ErrNotFound) {
			t.Errorf("RequestDelete(%q) = %v, want ErrNotFound", id, err)
		}
	}
}

// Every spelling of a UUID names the same session, in every store.
func otherIDSpellingsFindTheSession(t *testing.T, st session.Store) {
	s := mustAdmit(t, st, alice, home, roomy)
	ctx := context.Background()
	for _, id := range []string{strings.ToUpper(s.ID), "{" + s.ID + "}", strings.ReplaceAll(s.ID, "-", "")} {
		got, err := st.Get(ctx, id, alice)
		if err != nil || got.ID != s.ID {
			t.Errorf("Get(%q) = %q, %v; want %q", id, got.ID, err, s.ID)
		}
	}
	if err := st.RequestDelete(ctx, strings.ToUpper(s.ID), alice); err != nil {
		t.Errorf("RequestDelete(upper case) = %v, want nil", err)
	}
}
