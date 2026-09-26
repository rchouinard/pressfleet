// Package jobs runs the control plane's long-running work: a job queue in
// the state database, a worker pool, a step framework with rollback, and a
// scheduler for recurring jobs (design doc §3 Job Queue, §5).
//
// # Jobs
//
// A job has a type, a target (e.g. a site), a JSON payload, and an optional
// lock key. [Enqueue] inserts one; pass a transaction's Querier to enqueue
// atomically with other changes, as site provisioning does when it inserts
// the site row. Payloads, step data, and results are stored in plaintext and
// must never contain secrets.
//
// # Lifecycle
//
//	queued ──lease──▶ running ──all steps done──▶ succeeded
//	  ▲                  │
//	  └──retryable error─┤ (after backoff, while attempts remain)
//	                     │
//	                     └──permanent error or attempts used up──▶ rolling_back
//	                                                                 │
//	                             failed ◀──every Undo succeeded──────┤
//	                    rollback_failed ◀──an Undo failed for good───┘
//
// failed means the job gave up and rolled back what it created;
// rollback_failed means rolling back also failed, so resources may be left
// behind and an operator must look (the step errors say which). Both are
// final, as is succeeded. Attempts counts leases in the current phase: a
// failing job gets MaxAttempts runs, then its rollback gets MaxAttempts runs
// of its own.
//
// # Leasing
//
// A worker leases a job for a visibility timeout (Options.LeaseDuration)
// and extends the lease with heartbeats while it works. If the worker dies,
// the lease expires and another worker takes the job over, resuming where
// the persisted step progress says it stopped. Every write a worker makes is
// conditional on still holding the lease, so a worker that lost its lease
// (say, after a long pause) can't overwrite its successor's progress.
//
// On PostgreSQL, leasing uses SELECT ... FOR UPDATE SKIP LOCKED, so workers
// in several control plane processes never wait on or double-lease a row.
// SQLite runs one write transaction at a time (the store opens IMMEDIATE
// transactions), which serializes leasing without extra locking; its lock
// file already limits it to one process.
//
// # Lock keys
//
// Jobs with the same non-empty lock key run one at a time, first in, first
// out: "site:<id>" for every job that changes a site, "repo:<group-id>" for
// jobs that need a Restic repository to themselves. A job keeps its key
// from its first lease until it reaches a final state, including while it
// waits to retry or rolls back, so a later job never runs between a job's
// attempts. A queued job also blocks the jobs behind it while it waits for
// its RunAfter time. Jobs without a key, or with different keys, run
// concurrently up to the pool's concurrency.
//
// # Steps
//
// A [Handler] declares a job type's ordered, named steps. Each [Step] has a
// Do and an optional Undo. Progress is persisted per step, so a job that is
// retried or taken over skips the steps that succeeded and runs the
// interrupted or failed step again.
//
// STEPS MUST BE IDEMPOTENT. A step can run more than once: after a crash,
// after a lost lease, after a retryable error, or because a shutdown
// interrupted it. Do must succeed when its work is already partly or fully
// done ("create database" when it exists), and Undo must succeed when its
// work was never done or already undone. Undo runs for every step that
// started, including the one that failed, because a failed step may have
// half-applied its change; it runs newest first.
//
// Steps pass data forward with [Run.Set] and [Run.Get], persisted in the
// job at once, so a step can record an ID it generated before using it and
// find the same ID when it runs again. Outputs for API clients go in the
// result ([Run.SetResult]); secret outputs, such as a generated WordPress
// admin password, go through the pool's [SecretStore] and only their ID is
// stored ([Run.SetSecretResult]). Step logs ([Run.Logf]) are stored with
// the job and shown to users: never format a revealed secret into them.
// [redact.Secret] values print as a placeholder, so pass secrets in that
// type. Error messages are stored and shown too, and must not carry secrets
// either.
//
// # Retries
//
// A step that returns an error is retried with backoff (Options.Backoff)
// until the job has used MaxAttempts; wrap an error with [Permanent] to skip
// the retries when trying again can't help, such as invalid input. Either
// way, the job then rolls back. A panicking step fails permanently.
//
// # Shutdown
//
// When the pool's context is canceled it stops leasing. Running steps get
// Options.ShutdownGrace to finish; no new step starts. Jobs are then
// released for the next process to take over at once, without using up an
// attempt. If a step is still running when the grace period ends, its
// context is canceled; if the process dies instead, the job's lease expires
// and it is taken over then.
//
// # Schedules
//
// A [Scheduler] enqueues jobs from the recurring schedules in the database
// (store.Schedule): an interval with a per-schedule stagger derived from the
// schedule's name, so fifty sites' WP-Cron schedules don't fire in the same
// second, and a missed-run policy for slots that passed while no control
// plane was running. Claiming a due slot is a conditional UPDATE in the same
// transaction as the enqueue, so each slot fires once even with several
// control plane processes on one PostgreSQL database. Fixed intervals cover
// the MVP's schedules (WP-Cron every few minutes, daily backups, weekly
// prune, per-minute uptime checks) without a cron-expression dependency.
package jobs
