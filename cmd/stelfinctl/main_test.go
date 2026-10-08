package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/internal/pgtest"
	"github.com/stelfin/stelfin/ledger"
	"github.com/stelfin/stelfin/ledger/store"
)

var db *store.Store

func TestMain(m *testing.M) {
	pg, err := pgtest.Start(54417, ledger.Migrate)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	db = store.New(pg.Pool)
	code := m.Run()
	_ = pg.Stop()
	os.Exit(code)
}

// TestSuspendingIsWhatTheRouterReads: the router refuses a suspended workspace
// by reading orgs.status, and until this command existed nothing could set it —
// a kill switch with no handle.
func TestSuspendingIsWhatTheRouterReads(t *testing.T) {
	ctx := context.Background()
	org, err := db.CreateOrg(ctx, store.CreateOrgParams{
		Slug: "ctl-acme", DisplayName: "Acme", Network: "testnet",
		Channel: chat.Telegram, SpaceID: "-470001",
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	id := fmt.Sprint(int64(org.ID))

	var out bytes.Buffer
	if err := run(ctx, db, []string{"suspend", id}, &out); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if got, _ := db.Org(ctx, org.ID); got.Active() {
		t.Fatal("the org is still active after being suspended")
	}

	out.Reset()
	if err := run(ctx, db, []string{"orgs"}, &out); err != nil {
		t.Fatalf("orgs: %v", err)
	}
	if !strings.Contains(out.String(), id+"\tsuspended") {
		t.Fatalf("the listing does not show it suspended:\n%s", out.String())
	}

	if err := run(ctx, db, []string{"resume", id}, &out); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got, _ := db.Org(ctx, org.ID); !got.Active() {
		t.Fatal("the org is still suspended after being resumed")
	}
}

// The platform's own org holds the deployment's float and is nobody's tenant.
// Suspending it is not a thing that means anything, so it is not a thing this
// can do.
func TestThePlatformOrgCannotBeSuspended(t *testing.T) {
	ctx := context.Background()
	platform, err := db.EnsurePlatformOrg(ctx, "testnet")
	if err != nil {
		t.Fatalf("platform org: %v", err)
	}
	err = run(ctx, db, []string{"suspend", fmt.Sprint(int64(platform.ID))}, &bytes.Buffer{})
	if !errors.Is(err, store.ErrOrgNotFound) {
		t.Fatalf("error = %v, want ErrOrgNotFound", err)
	}
}

func TestBadArgumentsAreUsageErrors(t *testing.T) {
	for _, args := range [][]string{nil, {"suspend"}, {"delete", "1"}, {"orgs", "extra"}} {
		if err := run(context.Background(), db, args, &bytes.Buffer{}); !errors.Is(err, errUsage) {
			t.Errorf("%v: error = %v, want usage", args, err)
		}
	}
	if err := run(context.Background(), db, []string{"suspend", "acme"}, &bytes.Buffer{}); err == nil {
		t.Error("a name was accepted as an org id")
	}
}
