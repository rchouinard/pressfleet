package siteimport

import (
	"bytes"
	"io"
	"regexp"
)

// SanitizeStats counts what SanitizeDump removed.
type SanitizeStats struct {
	// Definers counts removed DEFINER clauses.
	Definers int
	// Statements counts removed statements: CREATE DATABASE, USE, and
	// the GTID and binary log statements of MySQL dumps.
	Statements int
}

var (
	// definer matches a DEFINER clause with its account, quoted with
	// backquotes or single quotes or bare, as mysqldump, mariadb-dump, and
	// phpMyAdmin write them. DEFINER=CURRENT_USER is left alone: it names
	// no one else.
	definer = regexp.MustCompile("(?i)DEFINER\\s*=\\s*(?:`(?:[^`]|``)*`|'(?:[^']|'')*'|[A-Za-z0-9_.$-]+)\\s*@\\s*" +
		"(?:`(?:[^`]|``)*`|'(?:[^']|'')*'|[A-Za-z0-9_.%:-]+)\\s?")
	// dropStatement matches the start of a statement that names the
	// source's database or needs administrative privileges.
	dropStatement = regexp.MustCompile(`^(?i:CREATE\s+(?:DATABASE|SCHEMA)\b|USE\s|SET\s+@@(?:GLOBAL\.)?GTID_PURGED|SET\s+@@SESSION\.SQL_LOG_BIN|` +
		`SET\s+@@SESSION\.GTID_NEXT|SET\s+@MYSQLDUMP_TEMP_LOG_BIN|/\*!\d+\s+SET\s+@@GLOBAL\.GTID_PURGED)`)
)

// SanitizeDump writes the dump r reads to w, decompressed and without what
// would keep it from importing into another database as that database's
// own user; see the package documentation. Data lines pass through
// untouched.
func SanitizeDump(w io.Writer, r io.Reader) (SanitizeStats, error) {
	var stats SanitizeStats
	br, _, err := openDump(r)
	if err != nil {
		return stats, err
	}
	dropping := false
	var sc stmtScanner
	err = lines(br, func(chunk []byte, start bool) error {
		if dropping {
			// A dropped statement may span lines (a long GTID set).
			if sc.endsStatement(chunk) {
				dropping = false
			}
			return nil
		}
		if start && !isData(chunk) {
			line := bytes.TrimLeft(chunk, " \t")
			if dropStatement.Match(line) {
				stats.Statements++
				sc = stmtScanner{}
				dropping = !sc.endsStatement(chunk)
				return nil
			}
			if n := len(definer.FindAllIndex(chunk, -1)); n > 0 {
				stats.Definers += n
				chunk = definer.ReplaceAll(chunk, nil)
			}
		}
		_, err := w.Write(chunk)
		return err
	})
	return stats, err
}

// stmtScanner finds where a dropped statement ends. A statement ends at a
// semicolon followed by nothing but whitespace and comments, so
// `USE db; -- note` ends on its line. It carries string and comment state
// across lines because a dropped statement may span them (a GTID set is a
// string literal with embedded newlines), and a comment marker inside a
// string is not a comment.
type stmtScanner struct {
	quote byte // the open string or identifier quote, or 0
	block bool // inside a /* */ comment
	semi  bool // the last significant byte seen was a ';' outside any string
}

// endsStatement scans chunk and reports whether the statement ends at its
// end.
func (s *stmtScanner) endsStatement(chunk []byte) bool {
	for i := 0; i < len(chunk); i++ {
		c := chunk[i]
		switch {
		case s.block:
			if c == '*' && i+1 < len(chunk) && chunk[i+1] == '/' {
				s.block = false
				i++
			}
		case s.quote != 0:
			switch {
			case c == '\\' && s.quote != '`':
				i++ // an escaped byte, which may be the quote
			case c == s.quote:
				s.quote = 0 // a doubled quote reopens on the next byte
			}
		case c == '\'' || c == '"' || c == '`':
			s.quote, s.semi = c, false
		case c == '/' && i+1 < len(chunk) && chunk[i+1] == '*':
			s.block = true
			i++
		case c == '#':
			return s.semi
		case c == '-' && i+1 < len(chunk) && chunk[i+1] == '-' && (i+2 == len(chunk) || isSpace(chunk[i+2])):
			// MySQL needs whitespace after "--"; `a--1` is arithmetic.
			return s.semi
		case c == ';':
			s.semi = true
		case isSpace(c):
		default:
			s.semi = false
		}
	}
	return s.semi && s.quote == 0
}
