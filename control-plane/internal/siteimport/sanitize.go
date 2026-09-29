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
	dropStatement = regexp.MustCompile(`^(?i:CREATE\s+DATABASE\b|USE\s|SET\s+@@(?:GLOBAL\.)?GTID_PURGED|SET\s+@@SESSION\.SQL_LOG_BIN|` +
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
	err = lines(br, func(chunk []byte, start bool) error {
		if dropping {
			// A dropped statement may span lines (a long GTID set).
			if bytes.HasSuffix(bytes.TrimRight(chunk, " \t\r\n"), []byte(";")) {
				dropping = false
			}
			return nil
		}
		if start && !isData(chunk) {
			line := bytes.TrimLeft(chunk, " \t")
			if dropStatement.Match(line) {
				stats.Statements++
				dropping = !bytes.HasSuffix(bytes.TrimRight(chunk, " \t\r\n"), []byte(";"))
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
