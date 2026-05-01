package gopublish

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// versionInfoBody is a small helper to build a .info JSON body for
// a (version, sha, time) triple. Uses the same shape the proxy
// emits.
func versionInfoBody(version, hash, t string) string {
	return `{
		"Version": "` + version + `",
		"Time": "` + t + `",
		"Origin": {"VCS":"git","URL":"https://example.com/repo","Ref":"refs/tags/` + version + `","Hash":"` + hash + `"}
	}`
}

// versionInfoBodyNoOrigin is the pre-go-1.20 shape: Version + Time
// only, no Origin block.
func versionInfoBodyNoOrigin(version, t string) string {
	return `{"Version":"` + version + `","Time":"` + t + `"}`
}

// getPinTableValue extracts the version_pin_table compound value
// from a CollectionResult and unmarshals it into a typed
// VersionPinTableValue.
func getPinTableValue(t *testing.T, result anySignals) VersionPinTableValue {
	t.Helper()
	for _, s := range result.Signals() {
		if s.Type == "version_pin_table" {
			var v VersionPinTableValue
			require.NoError(t, json.Unmarshal(s.Value, &v))
			return v
		}
	}
	t.Fatal("version_pin_table signal not found in result")
	return VersionPinTableValue{}
}

// TestGopublishCollector_EmitsVersionPinTable_AllProxyOrigins:
// Happy path — every selected version has a complete Origin block.
// All pins land in Pins[]; missing/failed lists are empty.
func TestGopublishCollector_EmitsVersionPinTable_AllProxyOrigins(t *testing.T) {
	t.Parallel()

	const modulePath = "example.com/clean"
	fx := happyPathFixtures()
	fx.listBody = "v0.1.0\nv0.2.0\nv0.3.0\n"
	fx.versionInfos = map[string]string{
		"v0.1.0": versionInfoBody("v0.1.0", "1111111111111111111111111111111111111111", "2026-04-10T00:00:00Z"),
		"v0.2.0": versionInfoBody("v0.2.0", "2222222222222222222222222222222222222222", "2026-04-15T00:00:00Z"),
		"v0.3.0": versionInfoBody("v0.3.0", "3333333333333333333333333333333333333333", "2026-04-20T00:00:00Z"),
	}
	srv := fakeProxyAndSum(t, modulePath, fx)

	c := noJitterCollector(srv.URL)
	result, err := c.Collect(context.Background(), goEntity(modulePath))
	require.NoError(t, err)
	require.True(t, hasSignal(result, "version_pin_table"))

	val := getPinTableValue(t, result)
	assert.Equal(t, modulePath, val.ModulePath)
	assert.Equal(t, 3, val.VersionCountTotal)
	assert.Equal(t, 3, val.VersionCountProcessed)
	assert.Empty(t, val.MissingOriginVersions)
	assert.Empty(t, val.FetchFailedVersions)
	require.Len(t, val.Pins, 3)

	// Most-recent first ordering: v0.3.0, v0.2.0, v0.1.0.
	assert.Equal(t, "v0.3.0", val.Pins[0].Version)
	assert.Equal(t, "3333333333333333333333333333333333333333", val.Pins[0].SHA)
	assert.Equal(t, "proxy.golang.org", val.Pins[0].Source)
	assert.Equal(t, "2026-04-20T00:00:00Z", val.Pins[0].PublishedAt)

	assert.Equal(t, "v0.2.0", val.Pins[1].Version)
	assert.Equal(t, "v0.1.0", val.Pins[2].Version)
}

// TestGopublishCollector_EmitsVersionPinTable_PartialMissingOrigins:
// Some versions return 200 OK with empty Origin block (pre-Go-1.20
// publishes). Those land in MissingOriginVersions[]; the rest are
// pinned. Source-evolution will fall back to local refs/tags for
// missing-origin versions when assembling matrix rows.
func TestGopublishCollector_EmitsVersionPinTable_PartialMissingOrigins(t *testing.T) {
	t.Parallel()

	const modulePath = "example.com/mixed"
	fx := happyPathFixtures()
	fx.listBody = "v0.1.0\nv0.2.0\nv0.3.0\n"
	fx.versionInfos = map[string]string{
		"v0.1.0": versionInfoBodyNoOrigin("v0.1.0", "2024-06-01T00:00:00Z"), // pre-1.20
		"v0.2.0": versionInfoBody("v0.2.0", "2222222222222222222222222222222222222222", "2026-04-15T00:00:00Z"),
		"v0.3.0": versionInfoBody("v0.3.0", "3333333333333333333333333333333333333333", "2026-04-20T00:00:00Z"),
	}
	srv := fakeProxyAndSum(t, modulePath, fx)

	c := noJitterCollector(srv.URL)
	result, err := c.Collect(context.Background(), goEntity(modulePath))
	require.NoError(t, err)

	val := getPinTableValue(t, result)
	assert.Equal(t, 3, val.VersionCountProcessed)
	assert.Len(t, val.Pins, 2)
	assert.Equal(t, []string{"v0.1.0"}, val.MissingOriginVersions)
	assert.Empty(t, val.FetchFailedVersions)
}

// TestGopublishCollector_EmitsVersionPinTable_PartialFetchFailures:
// Some versions return 5xx or 404. Those land in
// FetchFailedVersions[]; the rest are pinned.
func TestGopublishCollector_EmitsVersionPinTable_PartialFetchFailures(t *testing.T) {
	t.Parallel()

	const modulePath = "example.com/flaky"
	fx := happyPathFixtures()
	fx.listBody = "v0.1.0\nv0.2.0\nv0.3.0\n"
	fx.versionInfos = map[string]string{
		"v0.1.0": versionInfoBody("v0.1.0", "1111111111111111111111111111111111111111", "2026-04-10T00:00:00Z"),
		"v0.2.0": "", // empty body; status override below makes it 503
		"v0.3.0": versionInfoBody("v0.3.0", "3333333333333333333333333333333333333333", "2026-04-20T00:00:00Z"),
	}
	fx.versionInfoStatuses = map[string]int{
		"v0.2.0": http.StatusServiceUnavailable,
	}
	srv := fakeProxyAndSum(t, modulePath, fx)

	c := noJitterCollector(srv.URL)
	result, err := c.Collect(context.Background(), goEntity(modulePath))
	require.NoError(t, err)

	val := getPinTableValue(t, result)
	assert.Equal(t, 3, val.VersionCountProcessed)
	assert.Len(t, val.Pins, 2)
	assert.Empty(t, val.MissingOriginVersions)
	assert.Equal(t, []string{"v0.2.0"}, val.FetchFailedVersions)
}

// TestGopublishCollector_VersionPinTable_PreservesPublishOriginEmission:
// The new compound signal coexists with the existing per-@latest
// publish_origin signal. Other consumers of publish_origin must not
// regress.
func TestGopublishCollector_VersionPinTable_PreservesPublishOriginEmission(t *testing.T) {
	t.Parallel()

	const modulePath = "example.com/coexist"
	fx := happyPathFixtures()
	fx.listBody = "v0.20.0\n" // single version
	// happyPathFixtures() already sets lookupVersion="v0.20.0" and
	// the corresponding info body, so the legacy publish_origin
	// fixed-path handler answers. We add a versionInfos entry for
	// the same version so the prefix handler ALSO has the data
	// (though the fixed-path handler wins per ServeMux precedence).
	fx.versionInfos = map[string]string{
		"v0.20.0": fx.infoBody,
	}
	srv := fakeProxyAndSum(t, modulePath, fx)

	c := noJitterCollector(srv.URL)
	result, err := c.Collect(context.Background(), goEntity(modulePath))
	require.NoError(t, err)

	// Both signals should land.
	assert.True(t, hasSignal(result, "publish_origin"), "existing publish_origin emission must not regress")
	assert.True(t, hasSignal(result, "version_pin_table"), "new compound signal must emit")

	po := getSignalValue(t, result, "publish_origin")
	assert.Equal(t, "ec11c4a93de22cde2abe2bf74d70791033c2464c", po["hash"])

	val := getPinTableValue(t, result)
	require.Len(t, val.Pins, 1)
	assert.Equal(t, "v0.20.0", val.Pins[0].Version)
}

// TestGopublishCollector_VersionPinTable_EmptyVersionList_Absence:
// @v/list returned a successful but empty body. Pin table records
// an absence with a clear reason rather than emitting an empty
// pin set as a signal.
func TestGopublishCollector_VersionPinTable_EmptyVersionList_Absence(t *testing.T) {
	t.Parallel()

	const modulePath = "example.com/empty"
	fx := happyPathFixtures()
	fx.listBody = "" // proxy says module exists but no versions yet
	srv := fakeProxyAndSum(t, modulePath, fx)

	c := noJitterCollector(srv.URL)
	result, err := c.Collect(context.Background(), goEntity(modulePath))
	require.NoError(t, err)

	assert.False(t, hasSignal(result, "version_pin_table"))
	assert.True(t, hasAbsence(result, "version_pin_table"))
}

// TestGopublishCollector_VersionPinTable_VersionListNotFound_Absence:
// @v/list 404. The pin table is recorded as a non-retryable absence
// (matching version_count's behavior on the same condition).
func TestGopublishCollector_VersionPinTable_VersionListNotFound_Absence(t *testing.T) {
	t.Parallel()

	const modulePath = "example.com/missing"
	fx := happyPathFixtures()
	fx.listStatus = http.StatusNotFound
	fx.listBody = ""
	srv := fakeProxyAndSum(t, modulePath, fx)

	c := noJitterCollector(srv.URL)
	result, err := c.Collect(context.Background(), goEntity(modulePath))
	require.NoError(t, err)

	assert.False(t, hasSignal(result, "version_pin_table"))
	assert.True(t, hasAbsence(result, "version_pin_table"))
}

// TestGopublishCollector_VersionPinTable_CapsAtMostRecent12:
// A long-history module returns 30 versions in @v/list. Only the
// most-recent 12 are processed; the table records both the total
// (30) and the processed count (12).
func TestGopublishCollector_VersionPinTable_CapsAtMostRecent12(t *testing.T) {
	t.Parallel()

	const modulePath = "example.com/longhistory"

	// 30 versions, v0.1.0 through v0.30.0, with publish times
	// monotonically increasing.
	infoMap := make(map[string]string, 30)
	var listBuilder strings.Builder
	for i := 1; i <= 30; i++ {
		v := "v0." + itoa(i) + ".0"
		listBuilder.WriteString(v)
		listBuilder.WriteByte('\n')
		// Pad SHA out to 40 hex chars; use the version index as the
		// distinguishing prefix so we can identify which pins landed.
		sha := padSHA(itoa(i))
		// Synthetic timestamps; later index = later publish.
		ts := "2026-04-" + zeropad2(i) + "T00:00:00Z"
		infoMap[v] = versionInfoBody(v, sha, ts)
	}

	fx := happyPathFixtures()
	fx.listBody = listBuilder.String()
	fx.versionInfos = infoMap
	srv := fakeProxyAndSum(t, modulePath, fx)

	c := noJitterCollector(srv.URL)
	result, err := c.Collect(context.Background(), goEntity(modulePath))
	require.NoError(t, err)

	val := getPinTableValue(t, result)
	assert.Equal(t, 30, val.VersionCountTotal)
	assert.Equal(t, 12, val.VersionCountProcessed)
	require.Len(t, val.Pins, 12)

	// Most-recent first: v0.30.0, v0.29.0, ..., v0.19.0.
	assert.Equal(t, "v0.30.0", val.Pins[0].Version)
	assert.Equal(t, "v0.19.0", val.Pins[11].Version)
}

// itoa avoids strconv import for tiny loop body.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [3]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// zeropad2 returns n as a 2-character zero-padded string for dates.
func zeropad2(n int) string {
	if n < 10 {
		return "0" + itoa(n)
	}
	return itoa(n)
}

// padSHA pads prefix to 40 hex characters by appending zeros.
func padSHA(prefix string) string {
	out := prefix
	for len(out) < 40 {
		out += "0"
	}
	return out
}
