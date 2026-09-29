// Package siteimport reads the two files a site import starts from (#112,
// design doc §5.7): an archive of the site's wp-content directory and a SQL
// dump of its database. It checks them before anything is created and
// turns them into what the import job hands the platform: a plain tar of
// wp-content and a dump that imports as the site's own database user.
//
// Uploads only guarantee a file's size and checksum, so everything here
// treats both files as untrusted input.
//
// # The archive
//
// [InspectArchive] accepts a tar (optionally gzip- or bzip2-compressed) or
// zip archive and finds wp-content in it: a directory named wp-content at
// the top or one level down (an archive of wp-content itself, or of a
// whole WordPress install), or else the top of the archive, or its single
// top-level directory, if that holds themes, plugins, or uploads. Entries
// with absolute paths or ".." are refused outright, rather than skipped,
// since no honest export has them. Only regular files and directories are
// kept: symbolic links, hard links, and device files are skipped and
// reported.
//
// [WriteTar] writes wp-content's files as a plain tar with normalized
// modes, without the host-specific drop-ins (object-cache.php,
// advanced-cache.php, db.php), which the platform replaces with its own.
// Must-use plugins are kept; [ArchiveInfo.MUPlugins] lists them, so the job
// can warn that another host's may not work here.
//
// # The dump
//
// [InspectDump] accepts a plain or gzip-compressed SQL dump, as mysqldump,
// mariadb-dump, `wp db export`, and phpMyAdmin write them. It reads the
// tables the dump creates and, from the options table, the site's URLs and
// its database schema version (db_version), which says which WordPress
// core last upgraded it. It refuses dumps that aren't a readable SQL dump
// of one WordPress site: no WordPress tables, tables of several sites,
// multisite tables ([ErrMultisite]), or a missing site URL or db_version.
// The caller compares the schema version with its newest site image.
//
// Like mariadb-dump's own output (package sitedb/mariadb), dumps are read
// by line: dump tools write each statement's head at the start of a line
// and escape line breaks inside values, so a line that starts with INSERT,
// REPLACE, or "(" is data, and any other line start is SQL.
//
// [SanitizeDump] decompresses a dump and makes it importable into another
// database by an unprivileged user: it drops CREATE DATABASE and USE
// statements (a dump made with --databases names its source database),
// the DEFINER clauses of views, triggers, routines, and events (they name
// the source's accounts, and setting them needs privileges a site user
// doesn't have), and MySQL's GTID and binary log statements, which need
// administrative privileges.
package siteimport

import (
	"errors"
	"fmt"
)

// Sentinel errors. Every rejection wraps ErrInvalidArchive or
// ErrInvalidDump, with a message that says what is wrong and is safe to
// show the user.
var (
	// ErrInvalidArchive means the wp-content archive can't be imported.
	ErrInvalidArchive = errors.New("invalid wp-content archive")
	// ErrInvalidDump means the SQL dump can't be imported.
	ErrInvalidDump = errors.New("invalid SQL dump")
	// ErrMultisite means the dump is of a multisite network, which the
	// platform doesn't host. It wraps ErrInvalidDump.
	ErrMultisite = fmt.Errorf("%w: it is a WordPress multisite network, which can't be imported", ErrInvalidDump)
)

func invalidArchive(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidArchive, fmt.Sprintf(format, args...))
}

func invalidDump(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidDump, fmt.Sprintf(format, args...))
}
