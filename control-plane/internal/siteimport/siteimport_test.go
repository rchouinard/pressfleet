package siteimport_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/siteimport"
)

// file is an archive entry for the test archives; a name ending in "/" is
// a directory, and link, if set, makes it a symbolic link.
type file struct {
	name, body, link string
}

func tarFile(t *testing.T, gz bool, files ...file) *os.File {
	t.Helper()
	var buf bytes.Buffer
	var w io.Writer = &buf
	var zw *gzip.Writer
	if gz {
		zw = gzip.NewWriter(&buf)
		w = zw
	}
	tw := tar.NewWriter(w)
	for _, f := range files {
		h := &tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body)), Typeflag: tar.TypeReg}
		switch {
		case f.link != "":
			h.Typeflag, h.Linkname, h.Size = tar.TypeSymlink, f.link, 0
		case strings.HasSuffix(f.name, "/"):
			h.Typeflag, h.Mode, h.Size = tar.TypeDir, 0o755, 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(f.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if zw != nil {
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return tempFile(t, buf.Bytes())
}

func zipFile(t *testing.T, files ...file) *os.File {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		w, err := zw.Create(f.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return tempFile(t, buf.Bytes())
}

func tempFile(t *testing.T, data []byte) *os.File {
	t.Helper()
	p := filepath.Join(t.TempDir(), "upload")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// wpContent is a typical wp-content, under prefix.
func wpContent(prefix string) []file {
	return []file{
		{name: prefix + "themes/"},
		{name: prefix + "themes/twentytwentyfive/style.css", body: "/* Theme Name: TT5 */"},
		{name: prefix + "plugins/akismet/akismet.php", body: "<?php"},
		{name: prefix + "uploads/2026/09/photo.jpg", body: "jpeg"},
		{name: prefix + "mu-plugins/index.php", body: "<?php // Silence"},
		{name: prefix + "mu-plugins/kinsta-mu-plugins.php", body: "<?php"},
		{name: prefix + "mu-plugins/agency/agency.php", body: "<?php"},
		{name: prefix + "object-cache.php", body: "<?php // host cache"},
		{name: prefix + "advanced-cache.php", body: "<?php die('host');"},
		{name: prefix + "index.php", body: "<?php // Silence"},
	}
}

func TestInspectArchiveLayouts(t *testing.T) {
	for _, tt := range []struct {
		name     string
		f        func(t *testing.T) *os.File
		root     string
		format   string
		skipped  []string
		wantFile int
	}{
		{"tar.gz of wp-content", func(t *testing.T) *os.File { return tarFile(t, true, wpContent("wp-content/")...) }, "wp-content", siteimport.FormatTarGzip, nil, 7},
		{"tar of a whole install", func(t *testing.T) *os.File {
			return tarFile(t, false, append(wpContent("public_html/wp-content/"),
				file{name: "public_html/wp-config.php", body: "<?php"}, file{name: "public_html/wp-includes/version.php", body: "<?php"})...)
		}, "public_html/wp-content", siteimport.FormatTar, nil, 7},
		{"zip of wp-content's contents", func(t *testing.T) *os.File { return zipFile(t, wpContent("")...) }, "", siteimport.FormatZip, nil, 7},
		{"tar with ./ and a symlink", func(t *testing.T) *os.File {
			return tarFile(t, true, append(wpContent("./"), file{name: "./uploads/evil", link: "/etc/passwd"})...)
		}, "", siteimport.FormatTarGzip, []string{"uploads/evil"}, 7},
		{"single top-level directory", func(t *testing.T) *os.File { return zipFile(t, wpContent("backup-2026/")...) }, "backup-2026", siteimport.FormatZip, nil, 7},
	} {
		t.Run(tt.name, func(t *testing.T) {
			info, err := siteimport.InspectArchive(tt.f(t))
			if err != nil {
				t.Fatal(err)
			}
			if info.Root != tt.root || info.Format != tt.format || info.Files != tt.wantFile {
				t.Errorf("info = %+v", info)
			}
			if !slices.Equal(info.MUPlugins, []string{"agency", "kinsta-mu-plugins.php"}) {
				t.Errorf("MUPlugins = %v", info.MUPlugins)
			}
			if !slices.Equal(info.DropIns, []string{"advanced-cache.php", "object-cache.php"}) {
				t.Errorf("DropIns = %v", info.DropIns)
			}
			if !slices.Equal(info.Themes, []string{"twentytwentyfive"}) {
				t.Errorf("Themes = %v", info.Themes)
			}
			if !slices.Equal(info.Skipped, tt.skipped) {
				t.Errorf("Skipped = %v, want %v", info.Skipped, tt.skipped)
			}
		})
	}
}

func TestInspectArchiveRejects(t *testing.T) {
	for _, tt := range []struct {
		name, want string
		f          func(t *testing.T) *os.File
	}{
		{"not an archive", "isn't a tar or zip archive", func(t *testing.T) *os.File { return tempFile(t, []byte("-- MySQL dump\nCREATE TABLE x;")) }},
		{"empty file", "isn't a tar or zip", func(t *testing.T) *os.File { return tempFile(t, nil) }},
		{"xz", "isn't supported", func(t *testing.T) *os.File { return tempFile(t, []byte{0xfd, '7', 'z', 'X', 'Z', 0, 1, 2}) }},
		{"truncated gzip", "can't be", func(t *testing.T) *os.File {
			f := tarFile(t, true, wpContent("wp-content/")...)
			data, _ := io.ReadAll(f)
			return tempFile(t, data[:len(data)/2])
		}},
		{"path traversal", "leads outside", func(t *testing.T) *os.File {
			return tarFile(t, false, append(wpContent(""), file{name: "themes/../../etc/cron.d/x", body: "x"})...)
		}},
		{"absolute path", "absolute", func(t *testing.T) *os.File {
			return tarFile(t, false, append(wpContent(""), file{name: "/etc/x", body: "x"})...)
		}},
		{"not wp-content", "doesn't hold a wp-content", func(t *testing.T) *os.File {
			return zipFile(t, file{name: "docs/readme.txt", body: "x"}, file{name: "src/main.go", body: "x"})
		}},
		{"two wp-content", "several wp-content", func(t *testing.T) *os.File {
			return tarFile(t, false, append(wpContent("a/wp-content/"), wpContent("b/wp-content/")...)...)
		}},
		{"no files", "no files", func(t *testing.T) *os.File {
			return tarFile(t, false, file{name: "wp-content/"}, file{name: "wp-content/themes/"})
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := siteimport.InspectArchive(tt.f(t))
			if !errors.Is(err, siteimport.ErrInvalidArchive) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want ErrInvalidArchive saying %q", err, tt.want)
			}
		})
	}
}

func TestWriteTar(t *testing.T) {
	f := tarFile(t, true, append(wpContent("site/wp-content/"), file{name: "site/wp-content/uploads/link", link: "../x"},
		file{name: "site/wp-config.php", body: "<?php secret"})...)
	info, err := siteimport.InspectArchive(f)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := siteimport.WriteTar(&buf, f, info); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&buf)
	got := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		switch h.Typeflag {
		case tar.TypeDir:
			if h.Mode != 0o755 {
				t.Errorf("%s: mode %o", h.Name, h.Mode)
			}
		case tar.TypeReg:
			if h.Mode != 0o644 {
				t.Errorf("%s: mode %o", h.Name, h.Mode)
			}
		default:
			t.Errorf("%s: type %c", h.Name, h.Typeflag)
		}
		got[h.Name] = string(body)
	}
	for _, name := range []string{"themes/twentytwentyfive/style.css", "uploads/2026/09/photo.jpg", "mu-plugins/kinsta-mu-plugins.php", "index.php", "themes/"} {
		if _, ok := got[name]; !ok {
			t.Errorf("tar lacks %s: %v", name, got)
		}
	}
	for _, name := range []string{"object-cache.php", "advanced-cache.php", "uploads/link", "wp-config.php", "../wp-config.php"} {
		if _, ok := got[name]; ok {
			t.Errorf("tar has %s", name)
		}
	}
	if got["uploads/2026/09/photo.jpg"] != "jpeg" {
		t.Errorf("photo = %q", got["uploads/2026/09/photo.jpg"])
	}
}

// mysqldump is a dump in mysqldump's layout.
const mysqldump = "-- MySQL dump 10.13  Distrib 8.0.36\n" +
	"/*!40101 SET @OLD_CHARACTER_SET_CLIENT=@@CHARACTER_SET_CLIENT */;\n" +
	"SET @MYSQLDUMP_TEMP_LOG_BIN = @@SESSION.SQL_LOG_BIN;\n" +
	"SET @@SESSION.SQL_LOG_BIN= 0;\n" +
	"SET @@GLOBAL.GTID_PURGED=/*!80000 '+'*/ '3E11FA47-71CA-11E1-9E33-C80AA9429562:1-5,\n" +
	"3E11FA47-71CA-11E1-9E33-C80AA9429563:1-7';\n" +
	"CREATE DATABASE /*!32312 IF NOT EXISTS*/ `oldsite` /*!40100 DEFAULT CHARACTER SET utf8mb4 */;\n" +
	"USE `oldsite`;\n" +
	"DROP TABLE IF EXISTS `wpx7_options`;\n" +
	"CREATE TABLE `wpx7_options` (\n  `option_id` bigint unsigned NOT NULL AUTO_INCREMENT,\n  PRIMARY KEY (`option_id`)\n) ENGINE=InnoDB;\n" +
	"INSERT INTO `wpx7_options` VALUES (1,'siteurl','http://old.example/wp','yes'),(2,'home','http://old.example','yes')," +
	"(3,'blogname','Bob\\'s \\\"Blog\\\"','yes'),(4,'big','a:1:{s:1:\\\"x\\\";s:3:\\\"(;)\\\";}','no'),(5,'db_version','57155','yes')," +
	"(6,'stylesheet',_binary 'child','yes'),(7,'template','parent','yes');\n" +
	"CREATE TABLE `wpx7_posts` (`ID` bigint);\n" +
	"INSERT INTO `wpx7_posts` VALUES (1,'CREATE TABLE `wp_blogs` DEFINER=`root`@`localhost`');\n" +
	"CREATE TABLE `wpx7_users` (`ID` bigint);\n" +
	"CREATE TABLE `wpx7_usermeta` (`umeta_id` bigint);\n" +
	"/*!50001 CREATE ALGORITHM=UNDEFINED */\n" +
	"/*!50013 DEFINER=`root`@`localhost` SQL SECURITY DEFINER */\n" +
	"/*!50001 VIEW `wpx7_v` AS select 1 AS `x` */;\n" +
	"DELIMITER ;;\n" +
	"/*!50003 CREATE*/ /*!50017 DEFINER=`old user`@`%`*/ /*!50003 TRIGGER `t` BEFORE INSERT ON `wpx7_posts` FOR EACH ROW SET NEW.ID = NEW.ID */;;\n" +
	"DELIMITER ;\n" +
	"CREATE DEFINER='admin'@'10.0.0.%' PROCEDURE `p`() SELECT 1;\n"

// phpMyAdmin writes column lists and one row per line.
const phpMyAdmin = "-- phpMyAdmin SQL Dump\n" +
	"CREATE TABLE IF NOT EXISTS `site_options` (\n  `option_id` bigint(20)\n);\n" +
	"INSERT INTO `site_options` (`option_id`, `option_name`, `option_value`, `autoload`) VALUES\n" +
	"(1, 'siteurl', 'https://www.example.org', 'yes'),\n" +
	"(2, 'home', 'https://www.example.org/', 'yes'),\n" +
	"(3, 'db_version', '61833', 'yes'),\n" +
	"(4, 'blogname', 'It''s here', 'yes');\n" +
	"CREATE TABLE `site_posts` (`ID` int);\nCREATE TABLE `site_users` (`ID` int);\nCREATE TABLE `site_usermeta` (`ID` int);\n" +
	"CREATE TABLE `site_2_options` (`ID` int);\nCREATE TABLE `site_2_posts` (`ID` int);\n"

func gzipped(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestInspectDump(t *testing.T) {
	for _, tt := range []struct {
		name string
		dump []byte
		want siteimport.DumpInfo
	}{
		{"mysqldump", []byte(mysqldump), siteimport.DumpInfo{
			TablePrefix: "wpx7_", Tables: 4, Home: "http://old.example", SiteURL: "http://old.example/wp",
			DBVersion: 57155, BlogName: `Bob's "Blog"`, Stylesheet: "child",
		}},
		{"gzip", gzipped(t, mysqldump), siteimport.DumpInfo{
			Gzip: true, TablePrefix: "wpx7_", Tables: 4, Home: "http://old.example", SiteURL: "http://old.example/wp",
			DBVersion: 57155, BlogName: `Bob's "Blog"`, Stylesheet: "child",
		}},
		{"phpMyAdmin", []byte(phpMyAdmin), siteimport.DumpInfo{
			TablePrefix: "site_", Tables: 6, Home: "https://www.example.org", SiteURL: "https://www.example.org",
			DBVersion: 61833, BlogName: "It's here",
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := siteimport.InspectDump(bytes.NewReader(tt.dump))
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("InspectDump = %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestInspectDumpRejects(t *testing.T) {
	tables := "CREATE TABLE `wp_posts` (x int);\nCREATE TABLE `wp_users` (x int);\nCREATE TABLE `wp_usermeta` (x int);\nCREATE TABLE `wp_options` (x int);\n"
	options := "INSERT INTO `wp_options` VALUES (1,'siteurl','https://a.example','yes'),(2,'home','https://a.example','yes'),(3,'db_version','61833','yes');\n"
	for _, tt := range []struct {
		name, dump, want string
		multisite        bool
	}{
		{"empty", "", "empty", false},
		{"binary", "\x00\x01\x02garbage", "binary", false},
		{"zip", "PK\x03\x04rest", "isn't supported", false},
		{"no tables", "SELECT 1;\n", "creates no tables", false},
		{"not WordPress", "CREATE TABLE `orders` (x int);\n", "no WordPress tables", false},
		{"two sites", tables + strings.ReplaceAll(tables, "wp_", "b_") + options, "several WordPress sites (table prefixes b_, wp_)", false},
		{"multisite", tables + "CREATE TABLE `wp_blogs` (x int);\n" + options, "multisite", true},
		{"multisite sitemeta", tables + "CREATE TABLE `wp_sitemeta` (x int);\n" + options, "multisite", true},
		{"no siteurl", tables + "INSERT INTO `wp_options` VALUES (2,'home','https://a.example','yes'),(3,'db_version','1','yes');\n", "no siteurl", false},
		{"bad home", tables + "INSERT INTO `wp_options` VALUES (1,'siteurl','https://a.example','yes'),(2,'home','ftp://a','yes'),(3,'db_version','1','yes');\n", "isn't an http", false},
		{"no db_version", tables + "INSERT INTO `wp_options` VALUES (1,'siteurl','https://a.example','yes'),(2,'home','https://a.example','yes');\n", "no db_version", false},
		{"bad db_version", tables + strings.Replace(options, "'61833'", "'abc'", 1), "isn't a database version", false},
		{"unparsable insert", tables + "INSERT INTO `wp_options` VALUES (1,'siteurl') garbage;\n", "can't be parsed", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := siteimport.InspectDump(strings.NewReader(tt.dump))
			if !errors.Is(err, siteimport.ErrInvalidDump) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want ErrInvalidDump saying %q", err, tt.want)
			}
			if got := errors.Is(err, siteimport.ErrMultisite); got != tt.multisite {
				t.Errorf("ErrMultisite = %v, want %v", got, tt.multisite)
			}
		})
	}
	// A truncated gzip stream can't be read to the end.
	data := gzipped(t, mysqldump)
	if _, err := siteimport.InspectDump(bytes.NewReader(data[:len(data)-10])); !errors.Is(err, siteimport.ErrInvalidDump) {
		t.Errorf("truncated gzip: err = %v", err)
	}
}

func TestSanitizeDump(t *testing.T) {
	var out bytes.Buffer
	stats, err := siteimport.SanitizeDump(&out, bytes.NewReader(gzipped(t, mysqldump)))
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if stats.Definers != 3 || stats.Statements != 5 {
		t.Errorf("stats = %+v", stats)
	}
	for _, gone := range []string{"CREATE DATABASE", "USE `oldsite`", "GTID_PURGED", "3E11FA47-71CA-11E1-9E33-C80AA9429563", "SQL_LOG_BIN", "`old user`", "'admin'@"} {
		if strings.Contains(got, gone) {
			t.Errorf("sanitized dump still has %q:\n%s", gone, got)
		}
	}
	for _, kept := range []string{
		"/*!50013 SQL SECURITY DEFINER */",
		"/*!50003 CREATE*/ /*!50017 */ /*!50003 TRIGGER `t`",
		"CREATE PROCEDURE `p`() SELECT 1;",
		// Data is never touched.
		"INSERT INTO `wpx7_posts` VALUES (1,'CREATE TABLE `wp_blogs` DEFINER=`root`@`localhost`');",
		"DROP TABLE IF EXISTS `wpx7_options`;",
	} {
		if !strings.Contains(got, kept) {
			t.Errorf("sanitized dump lacks %q:\n%s", kept, got)
		}
	}
	// Sanitizing doesn't change what the dump says.
	if _, err := siteimport.InspectDump(strings.NewReader(got)); err != nil {
		t.Errorf("sanitized dump: %v", err)
	}
}
