package idempotency

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cadoranee/Doverka/internal/store"
)

func openIdempotencyTest(t *testing.T) (*store.DB, *Store) {
	t.Helper()
	if strings.TrimSpace(os.Getenv("DATABASE_URL")) == "" {
		t.Skip("DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := store.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		db.Close()
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, New(db.Pool())
}

func newTestIdempotencyKey(t *testing.T, label string) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return fmt.Sprintf("%s-%s", label, hex.EncodeToString(b[:]))
}

func cleanupIdempotencyKey(t *testing.T, db *store.DB, key string) {
	t.Helper()
	if key == "" {
		return
	}
	ctx := context.Background()
	_, err := db.Pool().Exec(ctx, `DELETE FROM idempotency_keys WHERE idempotency_key = $1`, key)
	if err != nil {
		t.Fatalf("cleanup idempotency key %s: %v", key, err)
	}
}

func TestBegin_sameKeyAndBodyReplaysTransferSnapshot(t *testing.T) {
	db, idem := openIdempotencyTest(t)
	key := newTestIdempotencyKey(t, "replay")
	t.Cleanup(func() { cleanupIdempotencyKey(t, db, key) })
	ctx := context.Background()
	body := []byte(`{"quote_id":"q-1"}`)
	snapshot := []byte(`{"transfer_id":"tr-1","status":"PROCESSING"}`)

	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	dec, err := idem.Begin(ctx, tx, key, body)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("Begin: %v", err)
	}
	if !dec.Claimed || dec.Replay != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("first Begin: Claimed=%v Replay=%v", dec.Claimed, dec.Replay)
	}
	if err := idem.Save(ctx, tx, key, 202, snapshot); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("Save: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	tx2, err := db.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx2.Rollback(ctx) }()
	dec2, err := idem.Begin(ctx, tx2, key, body)
	if err != nil {
		t.Fatalf("second Begin: %v", err)
	}
	if dec2.Claimed {
		t.Fatal("second Begin: Claimed, want false")
	}
	if dec2.Replay == nil {
		t.Fatal("second Begin: Replay is nil")
	}
	if dec2.Replay.StatusCode != 202 {
		t.Fatalf("Replay.StatusCode: got %d, want 202", dec2.Replay.StatusCode)
	}
	var replayed struct {
		TransferID string `json:"transfer_id"`
		Status     string `json:"status"`
	}
	if err := json.Unmarshal(dec2.Replay.Body, &replayed); err != nil {
		t.Fatalf("unmarshal replay: %v", err)
	}
	if replayed.TransferID != "tr-1" || replayed.Status != "PROCESSING" {
		t.Fatalf("replay body: got transfer_id=%q status=%q", replayed.TransferID, replayed.Status)
	}
}

func TestBegin_sameKeyDifferentBodyIsReused(t *testing.T) {
	db, idem := openIdempotencyTest(t)
	key := newTestIdempotencyKey(t, "reused")
	t.Cleanup(func() { cleanupIdempotencyKey(t, db, key) })
	ctx := context.Background()
	firstBody := []byte(`{"quote_id":"q-1"}`)
	snapshot := []byte(`{"transfer_id":"tr-1","status":"PROCESSING"}`)

	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	dec, err := idem.Begin(ctx, tx, key, firstBody)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("Begin: %v", err)
	}
	if !dec.Claimed {
		_ = tx.Rollback(ctx)
		t.Fatal("first Begin: not claimed")
	}
	if err := idem.Save(ctx, tx, key, 202, snapshot); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("Save: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	for _, tryBody := range [][]byte{
		[]byte(`{"quote_id":"q-2"}`),
		[]byte(`{"quote_id": "q-1"}`),
	} {
		txTry, err := db.Pool().Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		_, err = idem.Begin(ctx, txTry, key, tryBody)
		_ = txTry.Rollback(ctx)
		if !errors.Is(err, ErrReused) {
			t.Fatalf("Begin body %q: err=%v, want ErrReused", tryBody, err)
		}
	}
}

func TestBegin_parallelDoubleSubmitOnlyOneClaim(t *testing.T) {
	db, idem := openIdempotencyTest(t)
	key := newTestIdempotencyKey(t, "parallel")
	t.Cleanup(func() { cleanupIdempotencyKey(t, db, key) })
	body := []byte(`{"quote_id":"q-1"}`)
	snapshot := []byte(`{"transfer_id":"tr-1","status":"PROCESSING"}`)

	start := make(chan struct{})
	var wg sync.WaitGroup
	var claims atomic.Int32
	errs := make(chan error, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			tx, err := db.Pool().Begin(ctx)
			if err != nil {
				errs <- err
				return
			}
			dec, err := idem.Begin(ctx, tx, key, body)
			if err != nil {
				_ = tx.Rollback(ctx)
				errs <- err
				return
			}
			if dec.Claimed {
				claims.Add(1)
				if err := idem.Save(ctx, tx, key, 202, snapshot); err != nil {
					_ = tx.Rollback(ctx)
					errs <- err
					return
				}
				if err := tx.Commit(ctx); err != nil {
					errs <- err
					return
				}
				return
			}
			if dec.Replay == nil {
				_ = tx.Rollback(ctx)
				errs <- errors.New("replay missing")
				return
			}
			if dec.Replay.StatusCode != 202 {
				_ = tx.Rollback(ctx)
				errs <- fmt.Errorf("replay status %d", dec.Replay.StatusCode)
				return
			}
			var replayed struct {
				TransferID string `json:"transfer_id"`
				Status     string `json:"status"`
			}
			if err := json.Unmarshal(dec.Replay.Body, &replayed); err != nil {
				_ = tx.Rollback(ctx)
				errs <- err
				return
			}
			if replayed.TransferID != "tr-1" || replayed.Status != "PROCESSING" {
				_ = tx.Rollback(ctx)
				errs <- fmt.Errorf("replay body mismatch")
				return
			}
			_ = tx.Rollback(ctx)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("goroutine: %v", err)
		}
	}
	if claims.Load() != 1 {
		t.Fatalf("claims: got %d, want 1", claims.Load())
	}
}

func TestBegin_rollbackAllowsANewClaim(t *testing.T) {
	db, idem := openIdempotencyTest(t)
	key := newTestIdempotencyKey(t, "rollback-claim")
	t.Cleanup(func() { cleanupIdempotencyKey(t, db, key) })
	ctx := context.Background()
	body := []byte(`{"quote_id":"q-1"}`)

	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	dec, err := idem.Begin(ctx, tx, key, body)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("Begin: %v", err)
	}
	if !dec.Claimed {
		_ = tx.Rollback(ctx)
		t.Fatal("first Begin: not claimed")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	tx2, err := db.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx2.Rollback(ctx) }()
	dec2, err := idem.Begin(ctx, tx2, key, body)
	if err != nil {
		t.Fatalf("second Begin: %v", err)
	}
	if !dec2.Claimed {
		t.Fatal("second Begin: want claimed after rollback")
	}
}

func TestBegin_committedWithoutSnapshotIsInProgress(t *testing.T) {
	db, idem := openIdempotencyTest(t)
	key := newTestIdempotencyKey(t, "in-progress")
	t.Cleanup(func() { cleanupIdempotencyKey(t, db, key) })
	ctx := context.Background()
	body := []byte(`{"quote_id":"q-1"}`)

	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	dec, err := idem.Begin(ctx, tx, key, body)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("Begin: %v", err)
	}
	if !dec.Claimed {
		_ = tx.Rollback(ctx)
		t.Fatal("first Begin: not claimed")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	tx2, err := db.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx2.Rollback(ctx) }()
	_, err = idem.Begin(ctx, tx2, key, body)
	if !errors.Is(err, ErrInProgress) {
		t.Fatalf("second Begin: err=%v, want ErrInProgress", err)
	}
}

func TestSave_replaysStoredErrorSnapshot(t *testing.T) {
	db, idem := openIdempotencyTest(t)
	key := newTestIdempotencyKey(t, "error-snapshot")
	t.Cleanup(func() { cleanupIdempotencyKey(t, db, key) })
	ctx := context.Background()
	body := []byte(`{"quote_id":"q-1"}`)
	errSnapshot := []byte(`{"error":{"code":"INSUFFICIENT_FUNDS","message":"INSUFFICIENT_FUNDS"}}`)

	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	dec, err := idem.Begin(ctx, tx, key, body)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("Begin: %v", err)
	}
	if !dec.Claimed {
		_ = tx.Rollback(ctx)
		t.Fatal("Begin: not claimed")
	}
	if err := idem.Save(ctx, tx, key, 422, errSnapshot); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("Save: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	tx2, err := db.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx2.Rollback(ctx) }()
	dec2, err := idem.Begin(ctx, tx2, key, body)
	if err != nil {
		t.Fatalf("replay Begin: %v", err)
	}
	if dec2.Replay == nil || dec2.Replay.StatusCode != 422 {
		t.Fatalf("replay: got %+v", dec2.Replay)
	}
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(dec2.Replay.Body, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Error.Code != "INSUFFICIENT_FUNDS" {
		t.Fatalf("error.code: got %q", payload.Error.Code)
	}
}

func TestSave_rejectsMissingClaimAndSecondWrite(t *testing.T) {
	db, idem := openIdempotencyTest(t)
	key := newTestIdempotencyKey(t, "once")
	t.Cleanup(func() { cleanupIdempotencyKey(t, db, key) })
	ctx := context.Background()
	body := []byte(`{"quote_id":"q-1"}`)
	snapshot := []byte(`{"transfer_id":"tr-1","status":"PROCESSING"}`)

	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	err = idem.Save(ctx, tx, key, 202, snapshot)
	if !errors.Is(err, ErrNotClaimed) {
		_ = tx.Rollback(ctx)
		t.Fatalf("Save without claim: err=%v, want ErrNotClaimed", err)
	}
	dec, err := idem.Begin(ctx, tx, key, body)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("Begin: %v", err)
	}
	if !dec.Claimed {
		_ = tx.Rollback(ctx)
		t.Fatal("Begin: not claimed")
	}
	if err := idem.Save(ctx, tx, key, 202, snapshot); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("Save: %v", err)
	}
	err = idem.Save(ctx, tx, key, 202, snapshot)
	_ = tx.Rollback(ctx)
	if !errors.Is(err, ErrNotClaimed) {
		t.Fatalf("second Save: err=%v, want ErrNotClaimed", err)
	}
}

func TestBegin_differentKeysAreIndependent(t *testing.T) {
	db, idem := openIdempotencyTest(t)
	key1 := newTestIdempotencyKey(t, "key-a")
	key2 := newTestIdempotencyKey(t, "key-b")
	t.Cleanup(func() { cleanupIdempotencyKey(t, db, key1) })
	t.Cleanup(func() { cleanupIdempotencyKey(t, db, key2) })
	ctx := context.Background()
	body := []byte(`{"quote_id":"q-1"}`)

	claimKey := func(key string) {
		tx, err := db.Pool().Begin(ctx)
		if err != nil {
			t.Fatalf("begin %s: %v", key, err)
		}
		dec, err := idem.Begin(ctx, tx, key, body)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("Begin %s: %v", key, err)
		}
		if !dec.Claimed {
			_ = tx.Rollback(ctx)
			t.Fatalf("Begin %s: not claimed", key)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit %s: %v", key, err)
		}
	}
	claimKey(key1)
	claimKey(key2)
}

func TestBegin_rejectsEmptyKeyAndNilTransaction(t *testing.T) {
	ctx := context.Background()
	body := []byte(`{"quote_id":"q-1"}`)

	var nilStore *Store
	_, err := nilStore.Begin(ctx, nil, "k", body)
	if err == nil || err.Error() != "idempotency store is not configured" {
		t.Fatalf("nil Store: err=%v", err)
	}

	_, err = New(nil).Begin(ctx, nil, "k", body)
	if err == nil || err.Error() != "idempotency store is not configured" {
		t.Fatalf("nil pool: err=%v", err)
	}

	_, err = (&Store{}).Begin(ctx, nil, "k", body)
	if err == nil || err.Error() != "idempotency store is not configured" {
		t.Fatalf("zero Store: err=%v", err)
	}

	if strings.TrimSpace(os.Getenv("DATABASE_URL")) == "" {
		return
	}
	db, idem := openIdempotencyTest(t)
	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = idem.Begin(ctx, nil, "k", body)
	if err == nil || err.Error() != "transaction is nil" {
		t.Fatalf("nil tx: err=%v", err)
	}
	_, err = idem.Begin(ctx, tx, "", body)
	if err == nil || err.Error() != "idempotency key is empty" {
		t.Fatalf("empty key: err=%v", err)
	}
}
