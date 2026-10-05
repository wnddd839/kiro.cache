package sessionpin_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"kiro-go/internal/sessionpin"
)

func TestConversationIDStableWithinTTL(t *testing.T) {
	table := sessionpin.NewConversationTable(45 * time.Minute)
	first := table.ID("hdr:conv-1", "account-a")
	if first == "" {
		t.Fatal("expected non-empty conversation id")
	}
	for range 3 {
		if got := table.ID("hdr:conv-1", "account-a"); got != first {
			t.Fatalf("conversation id changed within ttl: %s vs %s", got, first)
		}
	}
}

func TestConversationIDFormat(t *testing.T) {
	table := sessionpin.NewConversationTable(0)
	id := table.ID("hdr:conv", "account-a")
	parts := strings.Split(id, "-")
	if len(parts) != 5 {
		t.Fatalf("uuid shape: %q", id)
	}
	if len(parts[0]) != 8 || len(parts[1]) != 4 || len(parts[2]) != 4 || len(parts[3]) != 4 || len(parts[4]) != 12 {
		t.Fatalf("uuid field widths: %q", id)
	}
	if id[14] != '4' {
		t.Fatalf("expected version nibble 4 (RFC 4122 v4), got %q", id)
	}
	if c := id[19]; c != '8' && c != '9' && c != 'a' && c != 'b' {
		t.Fatalf("expected variant nibble 8/9/a/b, got %q", id)
	}
}

func TestConversationIDDifferentKeysAndAccounts(t *testing.T) {
	table := sessionpin.NewConversationTable(0)
	base := table.ID("hdr:conv-1", "account-a")
	if base == table.ID("hdr:conv-2", "account-a") {
		t.Fatal("different sessions must not share an upstream conversation id")
	}
	if base == table.ID("hdr:conv-1", "account-b") {
		t.Fatal("account switch must mint a new upstream conversation id")
	}
}

// 会话在两个号之间来回：每个号保住自己的 conversationId，不因换号被重铸。
func TestConversationIDPerAccountSurvivesAlternation(t *testing.T) {
	table := sessionpin.NewConversationTable(time.Hour)
	a := table.ID("key", "account-a")
	b := table.ID("key", "account-b")
	if a == b {
		t.Fatal("different accounts must have different conversation ids")
	}
	for range 3 {
		if got := table.ID("key", "account-a"); got != a {
			t.Fatalf("account-a id re-minted after alternation: %s vs %s", got, a)
		}
		if got := table.ID("key", "account-b"); got != b {
			t.Fatalf("account-b id re-minted after alternation: %s vs %s", got, b)
		}
	}
}

func TestConversationForgetAccountOnlyDropsThatAccount(t *testing.T) {
	table := sessionpin.NewConversationTable(time.Hour)
	a := table.ID("key", "account-a")
	b := table.ID("key", "account-b")
	table.ForgetAccount("key", "account-a")
	if got := table.ID("key", "account-a"); got == a {
		t.Fatal("ForgetAccount must rotate that account's id")
	}
	if got := table.ID("key", "account-b"); got != b {
		t.Fatal("ForgetAccount must keep other accounts' ids")
	}
}

// Forget 作废所有号。
func TestConversationForgetDropsAllAccounts(t *testing.T) {
	table := sessionpin.NewConversationTable(time.Hour)
	a := table.ID("key", "account-a")
	b := table.ID("key", "account-b")
	table.Forget("key")
	if table.ID("key", "account-a") == a || table.ID("key", "account-b") == b {
		t.Fatal("Forget must rotate every account's id")
	}
}

func TestConversationIDRotatesAfterIdleExpiry(t *testing.T) {
	table := sessionpin.NewConversationTable(time.Millisecond)
	original := table.ID("key", "account-a")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got := table.ID("key", "account-a"); got != original {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("expired entry must rotate to a fresh id")
}

func TestConversationIDIsConcurrencySafe(t *testing.T) {
	table := sessionpin.NewConversationTable(0)
	const workers = 16
	ids := make([]string, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Go(func() {
			ids[i] = table.ID("shared-key", "account-a")
		})
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatal("parallel first requests for one session must share one id")
		}
	}
}

func TestConversationIDEmptyKeyOrAccountFallsBack(t *testing.T) {
	table := sessionpin.NewConversationTable(0)
	if got := table.ID("", "account-a"); got != "" {
		t.Fatalf("empty key must return empty id, got %q", got)
	}
	if got := table.ID("key", " "); got != "" {
		t.Fatalf("blank account must return empty id, got %q", got)
	}
	var nilTable *sessionpin.ConversationTable
	if got := nilTable.ID("key", "account-a"); got != "" {
		t.Fatalf("nil table must return empty id, got %q", got)
	}
}

func TestConversationTableEvictsOldestAtCapacity(t *testing.T) {
	table := sessionpin.NewConversationTable(time.Hour)
	const capacity = 4096
	first := table.ID("filler-0", "account-a")
	time.Sleep(5 * time.Millisecond)
	for i := 1; i < capacity; i++ {
		table.ID(fmt.Sprintf("filler-%d", i), "account-a")
	}
	table.ID("overflow-key", "account-a")
	if got := table.ID("filler-0", "account-a"); got == first {
		t.Fatal("oldest entry should have been evicted at capacity, id must rotate")
	}
}

func TestConversationForgetRotates(t *testing.T) {
	table := sessionpin.NewConversationTable(time.Hour)
	original := table.ID("key", "account-a")
	table.Forget("key")
	if got := table.ID("key", "account-a"); got == original {
		t.Fatal("forget must mint a fresh conversation id on the same account")
	}
}
