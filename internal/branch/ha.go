// SPDX-License-Identifier: AGPL-3.0-or-later

package branch

import (
	"fmt"
	"strings"
)

// HAState summarises high-availability status for the dashboard/API.
type HAState struct {
	Enabled   bool   `json:"enabled"`
	Standby   string `json:"standby"`   // standby container state
	Streaming bool   `json:"streaming"` // primary has a streaming standby
	Primary   string `json:"primary"`   // branch name currently serving as primary
}

// HAInfo reports current HA status.
func HAInfo() HAState {
	st := HAState{Primary: "main"}
	if PrimaryContainer() == container("standby") {
		st.Primary = "standby"
	}
	cs := ContainerState("standby")
	if cs == "absent" {
		return st
	}
	st.Enabled = true
	st.Standby = cs
	out, _ := capture("docker", "exec", "-e", "PGPASSWORD="+pgPassword, PrimaryContainer(),
		"psql", "-U", pgUser, "-d", pgDatabase, "-tAc",
		"SELECT count(*) FROM pg_stat_replication WHERE state='streaming';")
	if n := strings.TrimSpace(out); n != "" && n != "0" {
		st.Streaming = true
	}
	return st
}

// High availability: a hot standby that streams WAL from the primary
// (asynchronous, so it adds no commit latency), plus a manual failover that
// promotes the standby. The proxy routes "main" to whichever container is the
// current primary (see PrimaryContainer), so a promoted standby serves clients
// through the same endpoint.
//
// Note: this is a single-VM demonstration of the mechanism (replication +
// promotion + rerouting). Production HA additionally needs multi-host
// deployment, automatic failure detection, and fencing to prevent split-brain.

const (
	standbyDataset = "vectoradb/standby"
	standbyMount   = "/vectoradb/standby"
)

// HAEnable provisions a hot standby streaming from the current primary.
func HAEnable() error {
	primary := PrimaryContainer()

	// 1. Allow replication connections on the primary, then reload.
	quiet("docker", "exec", "-u", "postgres", primary, "bash", "-c",
		`grep -q '^host replication' "$PGDATA/pg_hba.conf" || echo 'host replication all all scram-sha-256' >> "$PGDATA/pg_hba.conf"`)
	if err := run("docker", "exec", "-e", "PGPASSWORD="+pgPassword, primary,
		"psql", "-U", pgUser, "-d", pgDatabase, "-c", "SELECT pg_reload_conf();"); err != nil {
		return err
	}

	// 2. Fresh standby storage.
	quiet("docker", "rm", "-f", container("standby"))
	store := activeStorage()
	if err := store.resetStandby(); err != nil {
		return err
	}
	if err := run("chown", "-R", pgUID+":"+pgUID, store.standbyPath()); err != nil {
		return err
	}

	// 3. Base backup from the primary, with recovery config (-R) and streaming WAL.
	if err := run("docker", "run", "--rm", "--user", pgUID, "--network", network,
		"-e", "PGPASSWORD="+pgPassword,
		"-v", store.standbyPath()+":/data",
		image,
		"pg_basebackup", "-h", primary, "-U", pgUser, "-D", "/data/pgdata",
		"-R", "-X", "stream", "-c", "fast"); err != nil {
		return err
	}

	// 4. Start the standby — it streams from the primary named in primary_conninfo.
	// Publish a host port so the proxy can route to it if it is later promoted.
	if err := run("docker", "run", "-d",
		"--name", container("standby"), "--network", network,
		"-p", "0:5432",
		"-e", "PGPASSWORD="+pgPassword, // used by the WAL receiver to authenticate
		"-e", "PGDATA=/var/lib/postgresql/data/pgdata",
		"-v", store.standbyPath()+":/var/lib/postgresql/data",
		image, "postgres", "-c", "listen_addresses=*"); err != nil {
		return err
	}
	return waitReady("standby")
}

// HAStatus prints replication status from both the primary and the standby.
func HAStatus() error {
	switch ContainerState("standby") {
	case "absent":
		fmt.Println("HA not enabled (no standby). Run: vectoradb ha enable")
		return nil
	case "running":
		// proceed
	default:
		fmt.Println("standby exists but is not running. Rebuild it with: vectoradb ha enable")
		return nil
	}
	fmt.Println("=== primary: connected standbys (pg_stat_replication) ===")
	_ = run("docker", "exec", "-e", "PGPASSWORD="+pgPassword, PrimaryContainer(),
		"psql", "-U", pgUser, "-d", pgDatabase, "-x", "-c",
		"SELECT application_name, client_addr, state, sync_state, replay_lag FROM pg_stat_replication;")
	fmt.Println("=== standby: recovery position ===")
	_ = run("docker", "exec", "-e", "PGPASSWORD="+pgPassword, container("standby"),
		"psql", "-U", pgUser, "-d", pgDatabase, "-c",
		"SELECT pg_is_in_recovery() AS in_recovery, pg_last_wal_receive_lsn() AS received, pg_last_wal_replay_lsn() AS replayed;")
	return nil
}

// HAFailover promotes the standby to primary and reroutes "main" to it. The old
// primary is stopped to avoid split-brain.
func HAFailover() error {
	if ContainerState("standby") != "running" {
		return fmt.Errorf("no running standby to promote — run 'vectoradb ha enable' first")
	}
	old := PrimaryContainer()
	if err := run("docker", "exec", "-e", "PGPASSWORD="+pgPassword, container("standby"),
		"psql", "-U", pgUser, "-d", pgDatabase, "-c", "SELECT pg_promote();"); err != nil {
		return err
	}
	quiet("docker", "stop", old) // the old primary steps down
	if err := setPrimary("standby"); err != nil {
		return err
	}
	fmt.Println("failover complete: standby promoted; 'main' now routes to it via the proxy")
	return nil
}

// HADisable tears down the standby and resets primary routing to "main".
func HADisable() error {
	quiet("docker", "rm", "-f", container("standby"))
	activeStorage().destroyStandby()
	return setPrimary("main")
}
