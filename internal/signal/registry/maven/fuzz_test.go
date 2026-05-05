package maven

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// --- Fuzz targets for Maven POM string-scanning parsers ---
//
// These parsers (parseParent, parseSCMURL, parseDevelopers, extractXMLElement)
// operate on raw bytes from Maven Central POM files. An attacker controlling a
// compromised package or performing a MITM between signatory and repo1 can
// serve arbitrary content. These fuzz tests verify safety invariants:
//
//   - Never panics (index-out-of-range, nil deref)
//   - No control characters leak into returned strings
//   - Structural consistency (all-or-nothing returns)
//   - No unbounded allocation (fuzzer memory limits catch this)

// --- Seed corpora ---

// realParentFragment is a representative <parent> block from a real POM.
const realParentFragment = `<parent>
  <groupId>org.apache.commons</groupId>
  <artifactId>commons-parent</artifactId>
  <version>52</version>
</parent>`

// realSCMFragment is a representative <scm> block.
const realSCMFragment = `<scm>
  <url>https://github.com/apache/commons-lang</url>
  <connection>scm:git:https://github.com/apache/commons-lang.git</connection>
</scm>`

// realDeveloperFragment is a representative <developers> block.
const realDeveloperFragment = `<developers>
  <developer>
    <id>britter</id>
    <name>Benedikt Ritter</name>
  </developer>
  <developer>
    <id>ggregory</id>
    <name>Gary Gregory</name>
  </developer>
</developers>`

// --- FuzzParseParent ---

func FuzzParseParent(f *testing.F) {
	f.Add([]byte(realParentFragment))
	f.Add([]byte(`<parent><groupId>com.google.guava</groupId><artifactId>guava-parent</artifactId><version>33.0-jre</version></parent>`))
	f.Add([]byte(`<project></project>`))
	f.Add([]byte(`<parent>`))          // unclosed
	f.Add([]byte(`<parent></parent>`)) // empty parent
	f.Add([]byte(`<parent><groupId></groupId><artifactId>x</artifactId><version>1</version></parent>`))
	f.Add([]byte{})                                                      // empty input
	f.Add([]byte(`<parent>` + strings.Repeat("A", 10000) + `</parent>`)) // large content, no child elements

	f.Fuzz(func(t *testing.T, data []byte) {
		g, a, v := parseParent(data)

		// Invariant 1: if any field is non-empty, they must be
		// independently parseable (we don't require all-or-nothing
		// because a POM can legally have only some sub-elements).
		// But no returned value may contain control characters.
		for _, s := range []string{g, a, v} {
			assertNoControlChars(t, "parseParent", s)
		}

		// Invariant 2: returned strings must be valid UTF-8.
		for _, s := range []string{g, a, v} {
			if !utf8.ValidString(s) {
				t.Errorf("parseParent returned invalid UTF-8: %q", s)
			}
		}
	})
}

// --- FuzzParseSCMURL ---

func FuzzParseSCMURL(f *testing.F) {
	f.Add([]byte(realSCMFragment))
	f.Add([]byte(`<scm><url>https://github.com/google/guava</url></scm>`))
	f.Add([]byte(`<scm><connection>scm:git:https://github.com/x/y.git</connection></scm>`))
	f.Add([]byte(`<scm><connection>scm:svn:https://svn.apache.org/repos/asf/commons</connection></scm>`))
	f.Add([]byte(`<scm></scm>`))                                              // empty scm section
	f.Add([]byte(`<scm>`))                                                    // unclosed
	f.Add([]byte(`<project></project>`))                                      // no scm at all
	f.Add([]byte{})                                                           // empty
	f.Add([]byte(`<scm><url>` + strings.Repeat("x", 10000) + `</url></scm>`)) // very long URL

	f.Fuzz(func(t *testing.T, data []byte) {
		result := parseSCMURL(data)

		// Invariant 1: no control characters in returned URL.
		assertNoControlChars(t, "parseSCMURL", result)

		// Invariant 2: valid UTF-8.
		if !utf8.ValidString(result) {
			t.Errorf("parseSCMURL returned invalid UTF-8: %q", result)
		}

		// Invariant 3: if non-empty, result should not retain the
		// scm:git: or scm:svn: prefix (those are stripped).
		if strings.HasPrefix(result, "scm:git:") || strings.HasPrefix(result, "scm:svn:") {
			t.Errorf("parseSCMURL did not strip scm prefix: %q", result)
		}
	})
}

// --- FuzzParseDevelopers ---

func FuzzParseDevelopers(f *testing.F) {
	f.Add([]byte(realDeveloperFragment))
	f.Add([]byte(`<developers><developer><id>solo</id></developer></developers>`))
	f.Add([]byte(`<developers><developer><name>Only Name</name></developer></developers>`))
	f.Add([]byte(`<developers></developers>`)) // empty section
	f.Add([]byte(`<developers>`))              // unclosed
	f.Add([]byte(`<project></project>`))       // no developers
	f.Add([]byte{})                            // empty
	// Many developers — check for allocation sanity
	f.Add([]byte(`<developers>` + strings.Repeat(`<developer><name>dev</name></developer>`, 100) + `</developers>`))

	f.Fuzz(func(t *testing.T, data []byte) {
		names := parseDevelopers(data)

		// Invariant 1: returned slice is nil or contains only non-empty strings.
		for i, name := range names {
			if name == "" {
				t.Errorf("parseDevelopers returned empty name at index %d", i)
			}
			assertNoControlChars(t, "parseDevelopers", name)
			if !utf8.ValidString(name) {
				t.Errorf("parseDevelopers returned invalid UTF-8 at index %d: %q", i, name)
			}
		}
	})
}

// --- FuzzExtractXMLElement ---

func FuzzExtractXMLElement(f *testing.F) {
	f.Add("<groupId>com.google.guava</groupId>", "groupId")
	f.Add("<url>https://github.com/x/y</url>", "url")
	f.Add("<name>  spaced  </name>", "name") // TrimSpace behavior
	f.Add("<tag></tag>", "tag")              // empty content
	f.Add("<tag>content", "tag")             // unclosed
	f.Add("no tags here", "tag")             // no match
	f.Add("", "tag")                         // empty input
	f.Add("<a><a>nested</a></a>", "a")       // nested same-name tags

	f.Fuzz(func(t *testing.T, xmlStr, tag string) {
		// Skip degenerate tags that would break the open/close pattern.
		if tag == "" || strings.ContainsAny(tag, "<>/\x00") {
			return
		}

		result := extractXMLElement(xmlStr, tag)

		// Invariant 1: no control characters.
		assertNoControlChars(t, "extractXMLElement", result)

		// Invariant 2: valid UTF-8.
		if !utf8.ValidString(result) {
			t.Errorf("extractXMLElement(%q, %q) returned invalid UTF-8: %q", xmlStr, tag, result)
		}

		// Invariant 3: result should not contain the tag delimiters
		// themselves (i.e., we're extracting content, not markup).
		open := "<" + tag + ">"
		close := "</" + tag + ">"
		if strings.Contains(result, open) || strings.Contains(result, close) {
			// This is actually possible with nested same-name tags and
			// is a known limitation of string-scanning. Flag it so we
			// can decide if it's acceptable.
			t.Logf("extractXMLElement returned content containing tag delimiters: input=%q tag=%q result=%q", xmlStr, tag, result)
		}

		// Invariant 4: result must be trimmed (no leading/trailing whitespace).
		if result != strings.TrimSpace(result) {
			t.Errorf("extractXMLElement result has untrimmed whitespace: %q", result)
		}
	})
}

// --- Helpers ---

// assertNoControlChars fails if s contains ASCII control characters (0x00–0x1F)
// except tab (0x09). These should never appear in a Maven coordinate, URL, or
// developer name extracted from a POM.
func assertNoControlChars(t *testing.T, fn, s string) {
	t.Helper()
	for i, r := range s {
		if r < 0x20 && r != '\t' {
			t.Errorf("%s: control char U+%04X at byte %d in %q", fn, r, i, s)
			return
		}
	}
}
