package session_test

import (
	"testing"
	"time"

	"github.com/os-playground-api/internal/session"
	"github.com/os-playground-api/internal/session/storetest"
)

func TestMemStoreContract(t *testing.T) {
	storetest.Run(t, func(*testing.T) session.Store { return session.NewMemStore(time.Now) })
}
