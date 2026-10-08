// Command stelfinctl is the operator's hands on a running deployment.
//
// It talks to the database and nothing else, and it does very little: list the
// workspaces, suspend one, bring one back. A suspended workspace keeps its
// history readable and moves no money through this deployment.
//
// This is deliberately not a chat command. The people who can suspend a
// workspace are the people operating the deployment; a command would make it
// something a compromised admin account could do to its own workspace at the
// worst moment, or be talked into. It needs the database URL, which is to say
// it needs to already be the operator.
//
//	stelfinctl orgs
//	stelfinctl suspend <org id>
//	stelfinctl resume  <org id>
//
// The deployment-wide switch is not here either: that is STELFIN_READONLY, in
// the server's environment, because the moment it is needed may be a moment
// the database is the thing that is not trusted.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stelfin/stelfin/ledger"
	"github.com/stelfin/stelfin/ledger/store"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dsn := strings.TrimSpace(os.Getenv("STELFIN_DATABASE_URL"))
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "stelfinctl: STELFIN_DATABASE_URL is not set")
		os.Exit(2)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stelfinctl: connect: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := run(ctx, store.New(pool), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "stelfinctl: %v\n", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

var errUsage = errors.New("usage: stelfinctl orgs | suspend <org id> | resume <org id>")

// run is main without the process around it, so it can be tested.
func run(ctx context.Context, db *store.Store, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errUsage
	}

	switch args[0] {
	case "orgs":
		if len(args) != 1 {
			return errUsage
		}
		orgs, err := db.Orgs(ctx)
		if err != nil {
			return err
		}
		if len(orgs) == 0 {
			fmt.Fprintln(out, "no workspaces yet")
			return nil
		}
		for _, o := range orgs {
			fmt.Fprintf(out, "%d\t%s\t%s\t%s\n", o.ID, o.Status, o.Network, o.DisplayName)
		}
		return nil

	case "suspend", "resume":
		if len(args) != 2 {
			return errUsage
		}
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || id <= 0 {
			return fmt.Errorf("%q is not an org id; `stelfinctl orgs` lists them", args[1])
		}
		status := store.OrgSuspended
		if args[0] == "resume" {
			status = store.OrgActive
		}
		if err := db.SetOrgStatus(ctx, ledger.OrgID(id), status); err != nil {
			return err
		}
		org, err := db.Org(ctx, ledger.OrgID(id))
		if err != nil {
			return err
		}
		// Takes effect on the next message: the router reads the org's status
		// for every one, so there is nothing to restart and no cache to wait on.
		fmt.Fprintf(out, "%d (%s) is now %s\n", org.ID, org.DisplayName, org.Status)
		return nil

	default:
		return errUsage
	}
}
