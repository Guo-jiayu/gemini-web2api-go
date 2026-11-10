package app

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func fairnessAccounts(t *testing.T, n int) []int64 {
	t.Helper()
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		id, err := accountAdd("fairness", fmt.Sprintf("SAPISID=fair%d; __Secure-1PSID=fair%d", i, i), "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		t.Cleanup(func() { _ = accountDelete(id) })
		if _, err := getDB().Exec(`UPDATE accounts SET last_used_at=? WHERE id=?`, time.Now().Unix(), id); err != nil {
			t.Fatal(err)
		}
	}
	return ids
}

func TestAccountRotationBalancedAcrossSameSecondBurst(t *testing.T) {
	ids := fairnessAccounts(t, 9)
	counts := map[int64]int{}
	for i := 0; i < 90; i++ {
		a, ok := pickCookieAccount()
		if !ok {
			t.Fatal("no account")
		}
		counts[a.ID]++
	}
	for _, id := range ids {
		if counts[id] != 10 {
			t.Fatalf("account %d selected %d times, expected 10; counts=%v", id, counts[id], counts)
		}
	}
}

func TestAccountRotationBalancedUnderConcurrency(t *testing.T) {
	ids := fairnessAccounts(t, 9)
	results := make(chan int64, 90)
	var wg sync.WaitGroup
	for i := 0; i < 90; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, ok := pickCookieAccount()
			if !ok {
				t.Error("no account")
				return
			}
			results <- a.ID
		}()
	}
	wg.Wait()
	close(results)
	counts := map[int64]int{}
	for id := range results {
		counts[id]++
	}
	for _, id := range ids {
		if counts[id] != 10 {
			t.Fatalf("uneven counts: %v", counts)
		}
	}
}

func TestAccountRotationRespectsEligibility(t *testing.T) {
	ids := fairnessAccounts(t, 6)
	if err := accountSetStatus(ids[0], "disabled"); err != nil {
		t.Fatal(err)
	}
	_, _ = getDB().Exec(`UPDATE accounts SET fail_count=1 WHERE id=?`, ids[1])
	counts := map[int64]int{}
	for i := 0; i < 30; i++ {
		a, ok := pickCookieAccountExcept(map[int64]bool{ids[2]: true})
		if !ok {
			t.Fatal("no account")
		}
		counts[a.ID]++
	}
	for _, id := range ids[3:] {
		if counts[id] != 10 {
			t.Fatalf("eligible accounts not balanced: %v", counts)
		}
	}
	if len(counts) != 3 {
		t.Fatalf("selected disabled, unhealthy or excluded account: %v", counts)
	}
}

func TestAccountRotationUsesPersistedOrder(t *testing.T) {
	ids := fairnessAccounts(t, 3)
	// Simulate state retained across a restart; all public timestamps are equal.
	for i, id := range ids {
		_, err := getDB().Exec(`UPDATE accounts SET pick_seq=? WHERE id=?`, 100+i, id)
		if err != nil {
			t.Fatal(err)
		}
	}
	a, ok := pickCookieAccount()
	if !ok || a.ID != ids[0] {
		t.Fatal("ignored persisted rotation order")
	}
	a, ok = pickCookieAccount()
	if !ok || a.ID != ids[1] {
		t.Fatal("did not advance persisted rotation order")
	}
}
