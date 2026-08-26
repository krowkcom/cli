package registry

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

// clock is a movable now, which is the only way a test reaches the expiry
// surface: nothing else can make 24 hours elapse inside one.
type clock struct{ at time.Time }

func (c *clock) now() time.Time          { return c.at }
func (c *clock) advance(d time.Duration) { c.at = c.at.Add(d) }

func newClockedServer(t *testing.T) (*httptest.Server, *clock) {
	t.Helper()
	c := &clock{at: time.Now()}
	server := httptest.NewServer(HandlerWithClock(0, "", c.now))
	t.Cleanup(server.Close)
	return server, c
}

// request is a bare HTTP call: these tests pin the server's contract, so they
// speak wire shapes rather than going through the client that already passes.
func request(t *testing.T, method, url, token, contentType, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var decoded map[string]any
	_ = json.NewDecoder(res.Body).Decode(&decoded)
	return res.StatusCode, decoded
}

// requestText is a plain keyless GET whose body is not JSON — the card page. An
// unfurler makes exactly this request and nothing else, so it is the one worth
// making here.
func requestText(t *testing.T, url string) (int, string) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, string(body)
}

// runSlugOf digs the slug out of the run an artifact reports. The run is a
// nested object rather than a bare slug, so a client reading an artifact back
// learns what produced it without a second call.
func runSlugOf(payload map[string]any) string {
	nested, _ := payload["run"].(map[string]any)
	slug, _ := nested["slug"].(string)
	return slug
}

func errorCode(payload map[string]any) string {
	e, _ := payload["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

// declare posts an artifact and returns its payload, keyless when token is "".
func declare(t *testing.T, server *httptest.Server, token, filename, body string) map[string]any {
	t.Helper()
	return declareTyped(t, server, token, filename, "text/plain", body)
}

// declareTyped is declare with the content type named, which matters wherever
// being an image is the thing under test — the card page's og:image is decided
// by it, and a screenshot declared as text/plain would quietly not be one.
func declareTyped(t *testing.T, server *httptest.Server, token, filename, contentType, body string) map[string]any {
	t.Helper()
	status, payload := request(t, http.MethodPost, server.URL+"/v1/artifacts", token, "application/json",
		fmt.Sprintf(`{"artifact":{"filename":%q,"content_type":%q,"byte_size":%d}}`,
			filename, contentType, len(body)))
	if status != http.StatusCreated {
		t.Fatalf("declare = %d %v", status, payload)
	}
	return payload
}

func uploadURL(t *testing.T, payload map[string]any) string {
	t.Helper()
	up, _ := payload["upload"].(map[string]any)
	url, _ := up["url"].(string)
	if url == "" {
		t.Fatalf("no upload url in %v", payload)
	}
	return url
}

func put(t *testing.T, url, body string) int {
	t.Helper()
	status, _ := request(t, http.MethodPut, url, "", "text/plain", body)
	return status
}

func finalize(t *testing.T, server *httptest.Server, token string, payload map[string]any) (int, map[string]any) {
	t.Helper()
	slug, _ := payload["slug"].(string)
	return request(t, http.MethodPut, server.URL+"/v1/artifacts/"+slug+"/finalization", token, "", "")
}

// putSigned uploads with exactly the headers the declare handed back, which is
// what a client does and what storage checks. put's fixed text/plain cannot
// carry a typed artifact: the content type is signed into the upload, so
// sending another one is refused the way a bad signature is.
func putSigned(t *testing.T, payload map[string]any, body string) int {
	t.Helper()
	upload, _ := payload["upload"].(map[string]any)
	headers, _ := upload["headers"].(map[string]any)
	req, err := http.NewRequest(http.MethodPut, uploadURL(t, payload), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range headers {
		if text, ok := value.(string); ok {
			req.Header.Set(name, text)
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	return res.StatusCode
}

// pngBytes is a real PNG of the given size, encoded rather than hand-built so
// the header under test is the one the standard library writes.
func pngBytes(t *testing.T, width, height int) string {
	t.Helper()
	var encoded strings.Builder
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return encoded.String()
}

// The registry measures an image at finalize and serves the pair on every read
// of it, so this has to as well. A client or a page that found dimensions here
// and none in production would have passed against a stand-in that drifted,
// which is the whole failure the shared contract exists to prevent.
func TestFinalizeMeasuresAnImageAndServesItsSize(t *testing.T) {
	server, _ := newClockedServer(t)
	body := pngBytes(t, 320, 200)

	payload := declareTyped(t, server, "krowk_sk_test", "shot.png", "image/png", body)
	// Nothing is measured before storage confirms the bytes, exactly as the size
	// on the record is the declared one until then.
	if payload["width"] != nil {
		t.Errorf("declared width = %v, want null", payload["width"])
	}
	if status := putSigned(t, payload, body); status != http.StatusOK {
		t.Fatalf("put = %d", status)
	}

	status, ready := finalize(t, server, "krowk_sk_test", payload)
	if status != http.StatusOK {
		t.Fatalf("finalize = %d %v", status, ready)
	}
	if ready["width"] != float64(320) || ready["height"] != float64(200) {
		t.Errorf("size = %vx%v, want 320x200", ready["width"], ready["height"])
	}
}

// WebP is the format the standard library cannot decode, so the stand-in reads
// its header by hand — and a hand-rolled parser is only worth trusting against
// bytes something else produced. These three are real files, one per encoding
// a WebP can hold, all 40x24.
func TestFinalizeMeasuresEveryWebPEncoding(t *testing.T) {
	for _, name := range []string{"tiny-vp8.webp", "tiny-vp8l.webp", "tiny-vp8x.webp"} {
		t.Run(name, func(t *testing.T) {
			server, _ := newClockedServer(t)
			raw, err := os.ReadFile(filepath.Join("testdata", name))
			if err != nil {
				t.Fatal(err)
			}
			body := string(raw)

			payload := declareTyped(t, server, "krowk_sk_test", name, "image/webp", body)
			if status := putSigned(t, payload, body); status != http.StatusOK {
				t.Fatalf("put = %d", status)
			}

			status, ready := finalize(t, server, "krowk_sk_test", payload)
			if status != http.StatusOK {
				t.Fatalf("finalize = %d %v", status, ready)
			}
			if ready["width"] != float64(40) || ready["height"] != float64(24) {
				t.Errorf("size = %vx%v, want 40x24", ready["width"], ready["height"])
			}
		})
	}
}

// A RIFF container that is not a WebP, and a WebP truncated to nothing useful,
// both have to fall through rather than read whatever is at those offsets.
func TestWebPSizeRefusesWhatItCannotRead(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "tiny-vp8.webp"))
	if err != nil {
		t.Fatal(err)
	}

	for _, spec := range []struct {
		name string
		body []byte
	}{
		{"too short to hold a header", raw[:20]},
		{"a RIFF that is not a WebP", append([]byte("RIFF____AVI "), raw[16:]...)},
		{"a lossy frame with no sync code", func() []byte {
			broken := append([]byte(nil), raw...)
			broken[23] = 0x00
			return broken
		}()},
	} {
		t.Run(spec.name, func(t *testing.T) {
			if width, height, ok := webPSize(spec.body); ok {
				t.Errorf("read %dx%d from bytes it should have refused", width, height)
			}
		})
	}
}

// The registry measures an SVG, so this does too. A percentage-sized one is
// deliberately not a measurement: it means "however big the box is", which is
// nothing a card can reserve space from.
func TestFinalizeMeasuresAnSvgOnlyWhenItStatesAFixedSize(t *testing.T) {
	for _, spec := range []struct {
		name          string
		attributes    string
		width, height any
	}{
		{"pixels", `width="120px" height="80px"`, float64(120), float64(80)},
		{"units", `width="120pt" height="80pt"`, float64(120), float64(80)},
		{"bare numbers", `width="120" height="80"`, float64(120), float64(80)},
		{"percentages", `width="100%" height="100%" viewBox="0 0 120 80"`, nil, nil},
		{"a viewBox alone", `viewBox="0 0 120 80"`, nil, nil},
	} {
		t.Run(spec.name, func(t *testing.T) {
			server, _ := newClockedServer(t)
			body := `<svg xmlns="http://www.w3.org/2000/svg" ` + spec.attributes + `></svg>`

			payload := declareTyped(t, server, "krowk_sk_test", "chart.svg", "image/svg+xml", body)
			if status := putSigned(t, payload, body); status != http.StatusOK {
				t.Fatalf("put = %d", status)
			}

			status, ready := finalize(t, server, "krowk_sk_test", payload)
			if status != http.StatusOK {
				t.Fatalf("finalize = %d %v", status, ready)
			}
			if ready["width"] != spec.width || ready["height"] != spec.height {
				t.Errorf("size = %vx%v, want %vx%v",
					ready["width"], ready["height"], spec.width, spec.height)
			}
		})
	}
}

// Nothing to measure is null, not a missing key and not a zero — an image zero
// pixels wide is a measurement, and this is the absence of one.
func TestArtifactWithNothingToMeasureSendsNullDimensions(t *testing.T) {
	server, _ := newClockedServer(t)

	for _, spec := range []struct{ name, filename, contentType, body string }{
		{"a file that is not an image", "notes.txt", "text/plain", "the bytes"},
		{"an image whose header says nothing", "shot.png", "image/png", "not an image"},
	} {
		t.Run(spec.name, func(t *testing.T) {
			payload := declareTyped(t, server, "krowk_sk_test", spec.filename, spec.contentType, spec.body)
			if status := putSigned(t, payload, spec.body); status != http.StatusOK {
				t.Fatalf("put = %d", status)
			}

			status, ready := finalize(t, server, "krowk_sk_test", payload)
			if status != http.StatusOK {
				t.Fatalf("finalize = %d %v", status, ready)
			}
			if ready["state"] != "ready" {
				t.Errorf("state = %v, want ready — a measurement must never fail a push", ready["state"])
			}
			width, present := ready["width"]
			if !present {
				t.Fatal("width is missing; it has to be present and null so a reader branches on the value")
			}
			if width != nil {
				t.Errorf("width = %v, want null", width)
			}
			if ready["height"] != nil {
				t.Errorf("height = %v, want null", ready["height"])
			}
		})
	}
}

// presign asks for the upload of an artifact again — keyless when token is "",
// and with no claim token at all when claimToken is.
func presign(t *testing.T, server *httptest.Server, token, slug, claimToken string) (int, map[string]any) {
	t.Helper()
	body := ""
	if claimToken != "" {
		body = fmt.Sprintf(`{"claim_token":%q}`, claimToken)
	}
	return request(t, http.MethodPost, server.URL+"/v1/artifacts/"+slug+"/upload",
		token, "application/json", body)
}

// The 24-hour promise and "claim before expiry or lose it" are core behaviour:
// past the lifetime, every endpoint that can meet the artifact answers 410.
func TestEphemeralExpiryAnswersGoneEverywhere(t *testing.T) {
	server, clk := newClockedServer(t)

	payload := declare(t, server, "", "a.txt", "the bytes")
	if put(t, uploadURL(t, payload), "the bytes") != http.StatusOK {
		t.Fatal("upload failed")
	}
	slug, _ := payload["slug"].(string)
	claimToken, _ := payload["claim_token"].(string)

	clk.advance(ephemeralLifetime + time.Minute)

	if status, body := request(t, http.MethodGet, server.URL+"/v1/artifacts/"+slug, "", "", ""); status != http.StatusGone || errorCode(body) != "expired" {
		t.Errorf("show after expiry = %d %v, want 410 expired", status, body)
	}
	if status, body := finalize(t, server, "", payload); status != http.StatusGone || errorCode(body) != "expired" {
		t.Errorf("finalize after expiry = %d %v, want 410 expired", status, body)
	}
	status, body := request(t, http.MethodPost, server.URL+"/v1/artifacts/"+slug+"/claim",
		"krowk_sk_test", "application/json", fmt.Sprintf(`{"claim_token":%q}`, claimToken))
	if status != http.StatusGone || errorCode(body) != "expired" {
		t.Errorf("claim after expiry = %d %v, want 410 expired", status, body)
	}
	if status, body := presign(t, server, "", slug, claimToken); status != http.StatusGone || errorCode(body) != "expired" {
		t.Errorf("represign after expiry = %d %v, want 410 expired", status, body)
	}
	// The promise covers the bytes too: the file URL stops serving, as the
	// real registry's lifecycle rule deletes the object.
	fileURL, _ := payload["file_url"].(string)
	if status, _ := request(t, http.MethodGet, fileURL, "", "", ""); status != http.StatusNotFound {
		t.Errorf("GET %s after expiry = %d, want 404", fileURL, status)
	}
	// The card page is what was pasted, so it does not go missing — it says the
	// artifact expired, which is a different thing to tell a reader than 404.
	card, _ := payload["url"].(string)
	if status, _ := request(t, http.MethodGet, card, "", "", ""); status != http.StatusGone {
		t.Errorf("GET %s after expiry = %d, want 410", card, status)
	}
}

func TestClaimJustBeforeExpiryStillSucceeds(t *testing.T) {
	server, clk := newClockedServer(t)

	payload := declare(t, server, "", "a.txt", "the bytes")
	claimToken, _ := payload["claim_token"].(string)
	slug, _ := payload["slug"].(string)

	clk.advance(ephemeralLifetime - time.Minute)

	status, body := request(t, http.MethodPost, server.URL+"/v1/artifacts/"+slug+"/claim",
		"krowk_sk_test", "application/json", fmt.Sprintf(`{"claim_token":%q}`, claimToken))
	if status != http.StatusOK {
		t.Fatalf("claim before expiry = %d %v", status, body)
	}
	// Claimed means kept: the artifact stops expiring.
	clk.advance(48 * time.Hour)
	if status, _ := request(t, http.MethodGet, server.URL+"/v1/artifacts/"+slug, "krowk_sk_test", "", ""); status != http.StatusOK {
		t.Errorf("claimed artifact expired anyway: %d", status)
	}
}

// The upload URL advertises a 15-minute expiry, and real storage enforces the
// signature's window — so the stand-in enforces it too.
func TestLateUploadIsRefused(t *testing.T) {
	server, clk := newClockedServer(t)

	payload := declare(t, server, "", "a.txt", "the bytes")
	clk.advance(UploadURLLifetime + time.Minute)

	if status := put(t, uploadURL(t, payload), "the bytes"); status != http.StatusForbidden {
		t.Errorf("late PUT = %d, want 403", status)
	}
}

// And the recovery from it, which is the whole reason the endpoint exists: a
// fresh URL over the same artifact. Declaring the file again would also produce
// one, and it would be a second slug — a dead link in whatever the first was
// pasted into.
func TestARepresignRecoversALapsedUpload(t *testing.T) {
	server, clk := newClockedServer(t)

	declared := declare(t, server, "krowk_sk_test", "a.txt", "the bytes")
	slug, _ := declared["slug"].(string)

	clk.advance(UploadURLLifetime + time.Minute)
	if status := put(t, uploadURL(t, declared), "the bytes"); status != http.StatusForbidden {
		t.Fatalf("late PUT = %d, want the 403 this recovers from", status)
	}

	status, fresh := presign(t, server, "krowk_sk_test", slug, "")
	if status != http.StatusOK {
		t.Fatalf("represign = %d %v, want 200", status, fresh)
	}
	// Nothing the link depends on moves: same slug, same URL, same declared size.
	// Only the signature is new.
	if fresh["slug"] != slug || fresh["url"] != declared["url"] || fresh["byte_size"] != declared["byte_size"] {
		t.Errorf("represign moved the artifact: %v, want %v", fresh, declared)
	}
	if uploadURL(t, fresh) == uploadURL(t, declared) {
		t.Error("the fresh upload URL is the one that just lapsed")
	}

	if status := put(t, uploadURL(t, fresh), "the bytes"); status != http.StatusOK {
		t.Fatalf("PUT to the fresh URL = %d, want 200", status)
	}
	if status, body := finalize(t, server, "krowk_sk_test", declared); status != http.StatusOK {
		t.Fatalf("finalize = %d %v", status, body)
	}
}

// The slug is the capability for *reading* a keyless artifact, because the bytes
// are public on the CDN regardless. A presigned PUT is not a read: it decides
// what those bytes are. A slug travels in whatever the link was pasted into, so
// on its own it must not buy the power to overwrite what somebody is reading.
func TestRepresigningAKeylessArtifactNeedsItsClaimToken(t *testing.T) {
	server, _ := newClockedServer(t)

	declared := declare(t, server, "", "a.txt", "the bytes")
	slug, _ := declared["slug"].(string)
	token, _ := declared["claim_token"].(string)

	if status, body := presign(t, server, "", slug, ""); status != http.StatusBadRequest ||
		errorCode(body) != "parameter_missing" {
		t.Errorf("represign on the slug alone = %d %v, want 400 parameter_missing", status, body)
	}
	if status, body := presign(t, server, "", slug, "krowk_claim_garbage"); status != http.StatusNotFound ||
		errorCode(body) != "not_found" {
		t.Errorf("represign with a wrong token = %d %v, want 404", status, body)
	}
	// A key is no authority over an artifact in the anonymous workspace either:
	// the lookup happens in that key's own workspace, where it does not exist.
	if status, _ := presign(t, server, "krowk_sk_test", slug, ""); status != http.StatusNotFound {
		t.Errorf("represign with an unrelated key = %d, want 404", status)
	}

	status, fresh := presign(t, server, "", slug, token)
	if status != http.StatusOK {
		t.Fatalf("represign with the token = %d %v", status, fresh)
	}
	if uploadURL(t, fresh) == uploadURL(t, declared) {
		t.Error("the fresh upload URL is the one it replaced")
	}
	// The token is shown once, by the call that minted it, and the row keeps only
	// a digest. Spending it here must not become a second chance to read it.
	if _, reissued := fresh["claim_token"]; reissued {
		t.Error("the represign re-issued the claim token")
	}
}

// A ready artifact is a permalink: a URL over its key would be permission to
// swap the bytes a link already resolves to.
func TestRepresigningAFinalizedArtifactIsRefused(t *testing.T) {
	server, _ := newClockedServer(t)

	declared := declare(t, server, "krowk_sk_test", "a.txt", "the bytes")
	slug, _ := declared["slug"].(string)
	if put(t, uploadURL(t, declared), "the bytes") != http.StatusOK {
		t.Fatal("upload failed")
	}
	if status, body := finalize(t, server, "krowk_sk_test", declared); status != http.StatusOK {
		t.Fatalf("finalize = %d %v", status, body)
	}

	status, body := presign(t, server, "krowk_sk_test", slug, "")
	if status != http.StatusConflict || errorCode(body) != "already_finalized" {
		t.Errorf("represign of a ready artifact = %d %v, want 409 already_finalized", status, body)
	}
}

// A real presigned URL signs the key: a valid token must not be able to land
// bytes under a different filename — or another workspace's prefix — and then
// finalize an artifact whose canonical link 404s.
func TestUploadTokenIsBoundToTheKey(t *testing.T) {
	server, _ := newClockedServer(t)

	payload := declare(t, server, "", "f.txt", "the bytes")
	rewritten := strings.Replace(uploadURL(t, payload), "/f.txt", "/other.png", 1)
	if status := put(t, rewritten, "the bytes"); status != http.StatusForbidden {
		t.Errorf("PUT to a rewritten filename = %d, want 403", status)
	}
	if status, body := finalize(t, server, "", payload); status != http.StatusConflict {
		t.Errorf("finalize after a refused upload = %d %v, want 409 upload_missing", status, body)
	}

	// The canonical key still works.
	if status := put(t, uploadURL(t, payload), "the bytes"); status != http.StatusOK {
		t.Errorf("canonical PUT = %d, want 200", status)
	}
}

// A claim answers 200 only to the artifact's own token — including on an
// artifact the workspace already holds, where a garbage token must not ride
// the retry-after-success affordance.
func TestClaimRequiresTheRealToken(t *testing.T) {
	server, _ := newClockedServer(t)
	claimPath := func(slug string) string { return server.URL + "/v1/artifacts/" + slug + "/claim" }

	// An artifact pushed with a key was never claimable: any token is a 404.
	owned := declare(t, server, "krowk_sk_test", "a.txt", "the bytes")
	ownedSlug, _ := owned["slug"].(string)
	if status, _ := request(t, http.MethodPost, claimPath(ownedSlug),
		"krowk_sk_test", "application/json", `{"claim_token":"krowk_claim_garbage"}`); status != http.StatusNotFound {
		t.Errorf("claiming a keyed artifact = %d, want 404", status)
	}

	anonymous := declare(t, server, "", "b.txt", "the bytes")
	slug, _ := anonymous["slug"].(string)
	token, _ := anonymous["claim_token"].(string)
	claim := func(key, claimToken string) int {
		status, _ := request(t, http.MethodPost, claimPath(slug),
			key, "application/json", fmt.Sprintf(`{"claim_token":%q}`, claimToken))
		return status
	}

	if got := claim("krowk_sk_test", token); got != http.StatusOK {
		t.Fatalf("claim = %d", got)
	}
	// A retry with the real token is the same success; a garbage token is not.
	if got := claim("krowk_sk_test", token); got != http.StatusOK {
		t.Errorf("retry with the real token = %d, want 200", got)
	}
	if got := claim("krowk_sk_test", "krowk_claim_garbage"); got != http.StatusNotFound {
		t.Errorf("retry with a garbage token = %d, want 404", got)
	}
	// The token was spent for everyone else.
	if got := claim("krowk_sk_other", token); got != http.StatusNotFound {
		t.Errorf("second workspace with the spent token = %d, want 404", got)
	}
}

// Claiming moves the artifact to a new workspace, but the bytes stay where the
// presigned URL was signed for — an upload URL issued before the claim must
// keep working, or a claimed pending artifact could never become ready.
func TestClaimDoesNotInvalidateTheUploadURL(t *testing.T) {
	server, _ := newClockedServer(t)

	payload := declare(t, server, "", "a.txt", "the bytes")
	slug, _ := payload["slug"].(string)
	token, _ := payload["claim_token"].(string)

	status, body := request(t, http.MethodPost, server.URL+"/v1/artifacts/"+slug+"/claim",
		"krowk_sk_test", "application/json", fmt.Sprintf(`{"claim_token":%q}`, token))
	if status != http.StatusOK {
		t.Fatalf("claim = %d %v", status, body)
	}

	if status := put(t, uploadURL(t, payload), "the bytes"); status != http.StatusOK {
		t.Fatalf("PUT after claim = %d, want the pre-claim URL to still work", status)
	}
	if status, body := finalize(t, server, "krowk_sk_test", payload); status != http.StatusOK {
		t.Errorf("finalize after claim = %d %v", status, body)
	}
}

// The stand-in must not be more forgiving than the real thing: a run body that
// does not parse is refused, exactly as a garbage artifact body is, and with
// the same code — the payload is what is broken, not one field in it.
func TestGarbageRunBodyIsRejected(t *testing.T) {
	server, _ := newClockedServer(t)

	status, body := request(t, http.MethodPost, server.URL+"/v1/runs",
		"krowk_sk_test", "application/json", `{"run": not json`)
	if status != http.StatusBadRequest || errorCode(body) != "bad_request" {
		t.Errorf("garbage run body = %d %v, want 400 bad_request", status, body)
	}
	// An empty body is still fine: a run needs no metadata.
	if status, _ := request(t, http.MethodPost, server.URL+"/v1/runs", "krowk_sk_test", "", ""); status != http.StatusCreated {
		t.Errorf("empty run body = %d, want 201", status)
	}
}

// Finalizing is documented as idempotent because agents retry: the second
// finalize is the same success carrying the same artifact, not an error and
// not a fresh state transition.
func TestFinalizeTwiceReturnsTheSameArtifact(t *testing.T) {
	server, _ := newClockedServer(t)

	payload := declare(t, server, "krowk_sk_test", "shot.txt", "the bytes")
	if status := put(t, uploadURL(t, payload), "the bytes"); status != http.StatusOK {
		t.Fatalf("put = %d", status)
	}

	status, first := finalize(t, server, "krowk_sk_test", payload)
	if status != http.StatusOK {
		t.Fatalf("first finalize = %d %v", status, first)
	}
	status, second := finalize(t, server, "krowk_sk_test", payload)
	if status != http.StatusOK {
		t.Fatalf("second finalize = %d %v", status, second)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("second finalize answered differently:\nfirst:  %v\nsecond: %v", first, second)
	}
	if first["state"] != "ready" {
		t.Errorf("state = %v, want ready", first["state"])
	}
}

// Completing a run is idempotent too, and the record keeps the moment it first
// finished — a CI cleanup step that runs twice gets the same success both
// times, with the same finished_at.
func TestFinishRunTwiceKeepsTheFirstFinishedAt(t *testing.T) {
	server, clk := newClockedServer(t)

	status, created := request(t, http.MethodPost, server.URL+"/v1/runs",
		"krowk_sk_test", "application/json", `{"run":{}}`)
	if status != http.StatusCreated {
		t.Fatalf("create run = %d %v", status, created)
	}
	slug, _ := created["slug"].(string)

	status, first := request(t, http.MethodPut, server.URL+"/v1/runs/"+slug+"/completion",
		"krowk_sk_test", "", "")
	if status != http.StatusOK || first["status"] != "finished" {
		t.Fatalf("first completion = %d %v", status, first)
	}
	finishedAt, _ := first["finished_at"].(string)
	if finishedAt == "" {
		t.Fatal("no finished_at on the first completion")
	}

	// Enough time passes for a second completion to be visibly a different
	// moment — the answer must keep the first one anyway.
	clk.advance(time.Hour)
	status, second := request(t, http.MethodPut, server.URL+"/v1/runs/"+slug+"/completion",
		"krowk_sk_test", "", "")
	if status != http.StatusOK {
		t.Fatalf("second completion = %d %v", status, second)
	}
	if got, _ := second["finished_at"].(string); got != finishedAt {
		t.Errorf("finished_at moved from %q to %q on a retry", finishedAt, got)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("second completion answered differently:\nfirst:  %v\nsecond: %v", first, second)
	}
}

// openRun opens a run and returns its slug.
func openRun(t *testing.T, server *httptest.Server, token string) string {
	t.Helper()
	status, payload := request(t, http.MethodPost, server.URL+"/v1/runs", token, "application/json", `{"run":{}}`)
	if status != http.StatusCreated {
		t.Fatalf("open run = %d %v", status, payload)
	}
	slug, _ := payload["slug"].(string)
	return slug
}

// Attaching a run after the fact is the route the claim flow needs: a keyless
// upload cannot name a run, and claiming it does not give it one.
func TestAttachRunPutsAnOwnedArtifactUnderARun(t *testing.T) {
	server, _ := newClockedServer(t)
	const token = "krowk_sk_owner"

	artifact := declare(t, server, token, "shot.png", "some bytes")
	slug, _ := artifact["slug"].(string)
	run := openRun(t, server, token)

	url := server.URL + "/v1/artifacts/" + slug + "/run"
	status, payload := request(t, http.MethodPut, url, token, "application/json",
		fmt.Sprintf(`{"run":%q}`, run))
	if status != http.StatusOK || runSlugOf(payload) != run {
		t.Fatalf("attach = %d %v, want the artifact under %s", status, payload, run)
	}

	// Idempotent, because agents retry: the same PUT is the same success.
	if again, body := request(t, http.MethodPut, url, token, "application/json",
		fmt.Sprintf(`{"run":%q}`, run)); again != http.StatusOK || runSlugOf(body) != run {
		t.Errorf("second attach = %d %v", again, body)
	}

	// PATCH is routed alongside PUT, as the registry's own routes are.
	if status, body := request(t, http.MethodPatch, url, token, "application/json",
		fmt.Sprintf(`{"run":%q}`, run)); status != http.StatusOK || runSlugOf(body) != run {
		t.Errorf("PATCH alias = %d %v", status, body)
	}

	// The link is what has already been pasted, so attaching must not move it.
	if shown := mustShow(t, server, token, slug); shown["url"] != artifact["url"] {
		t.Errorf("attaching moved the link: %v → %v", artifact["url"], shown["url"])
	}

	// A closed run still accepts one. The case this exists for is a CI run that
	// finished long before anyone claimed the anonymous upload it left behind, so
	// refusing here would leave that upload nowhere to go.
	if status, payload := request(t, http.MethodPut,
		server.URL+"/v1/runs/"+run+"/completion", token, "", ""); status != http.StatusOK {
		t.Fatalf("finish run = %d %v", status, payload)
	}
	second := declare(t, server, token, "later.png", "some bytes")
	laterSlug, _ := second["slug"].(string)
	if status, payload := request(t, http.MethodPut,
		server.URL+"/v1/artifacts/"+laterSlug+"/run", token, "application/json",
		fmt.Sprintf(`{"run":%q}`, run)); status != http.StatusOK || runSlugOf(payload) != run {
		t.Errorf("attach to a finished run = %d %v, want it allowed", status, payload)
	}
}

func mustShow(t *testing.T, server *httptest.Server, token, slug string) map[string]any {
	t.Helper()
	status, payload := request(t, http.MethodGet, server.URL+"/v1/artifacts/"+slug, token, "", "")
	if status != http.StatusOK {
		t.Fatalf("show = %d %v", status, payload)
	}
	return payload
}

// A run belongs to a workspace, so a keyless attach is refused with the same code
// a keyless push naming a run gets — and the run parameter is required.
func TestAttachRunNeedsAKeyAndARun(t *testing.T) {
	server, _ := newClockedServer(t)
	const token = "krowk_sk_owner"

	artifact := declare(t, server, token, "shot.png", "some bytes")
	slug, _ := artifact["slug"].(string)
	run := openRun(t, server, token)
	url := server.URL + "/v1/artifacts/" + slug + "/run"

	// A plain 401, the way the registry refuses this route — not the run_needs_key a
	// keyless push naming a run gets, which is raised only where a request may
	// legitimately arrive without a key.
	status, payload := request(t, http.MethodPut, url, "", "application/json", fmt.Sprintf(`{"run":%q}`, run))
	if status != http.StatusUnauthorized || errorCode(payload) != "unauthorized" {
		t.Errorf("keyless attach = %d %s, want 401 unauthorized", status, errorCode(payload))
	}

	status, payload = request(t, http.MethodPut, url, token, "application/json", `{}`)
	if status != http.StatusBadRequest || errorCode(payload) != "parameter_missing" {
		t.Errorf("attach with no run = %d %s, want 400 parameter_missing", status, errorCode(payload))
	}
}

// Both slugs resolve in the requesting key's workspace, so another workspace's
// artifact and another workspace's run are both simply not there. That is also
// what makes claim-then-attach the required order: an unclaimed anonymous upload
// is in nobody's workspace.
func TestAttachRunIsScopedToTheKeysWorkspace(t *testing.T) {
	server, _ := newClockedServer(t)
	const owner, stranger = "krowk_sk_owner", "krowk_sk_stranger"

	artifact := declare(t, server, owner, "shot.png", "some bytes")
	slug, _ := artifact["slug"].(string)
	run := openRun(t, server, owner)
	url := server.URL + "/v1/artifacts/" + slug + "/run"

	if status, payload := request(t, http.MethodPut, url, stranger, "application/json",
		fmt.Sprintf(`{"run":%q}`, run)); status != http.StatusNotFound || errorCode(payload) != "not_found" {
		t.Errorf("a stranger attached it: %d %s", status, errorCode(payload))
	}
	if status, payload := request(t, http.MethodPut, url, owner, "application/json",
		`{"run":"run_nosuchrunatall00000"}`); status != http.StatusNotFound || errorCode(payload) != "not_found" {
		t.Errorf("unknown run = %d %s, want 404 not_found", status, errorCode(payload))
	}
	// Someone else's run is the same answer, not a hint that it exists.
	strangersRun := openRun(t, server, stranger)
	if status, payload := request(t, http.MethodPut, url, owner, "application/json",
		fmt.Sprintf(`{"run":%q}`, strangersRun)); status != http.StatusNotFound {
		t.Errorf("attached to another workspace's run: %d %v", status, payload)
	}

	anonymous := declare(t, server, "", "anon.png", "some bytes")
	anonSlug, _ := anonymous["slug"].(string)
	if status, payload := request(t, http.MethodPut,
		server.URL+"/v1/artifacts/"+anonSlug+"/run", owner, "application/json",
		fmt.Sprintf(`{"run":%q}`, run)); status != http.StatusNotFound {
		t.Errorf("an unclaimed anonymous upload was attachable: %d %v", status, payload)
	}
}

// The real presign signs the digest as a header, so storage refuses the PUT
// outright when it is missing — the signature does not verify without it. The
// header therefore has to be handed over and has to be required, or a client
// that ignores upload.headers passes here and fails against R2.
func TestUploadRequiresTheChecksumHeaderItWasHanded(t *testing.T) {
	server, _ := newClockedServer(t)
	body := "the bytes"
	sum := sha256.Sum256([]byte(body))
	encoded := base64.StdEncoding.EncodeToString(sum[:])

	status, payload := request(t, http.MethodPost, server.URL+"/v1/artifacts", "", "application/json",
		fmt.Sprintf(`{"artifact":{"filename":"a.txt","content_type":"text/plain","byte_size":%d,"checksum":%q}}`,
			len(body), hex.EncodeToString(sum[:])))
	if status != http.StatusCreated {
		t.Fatalf("declare = %d %v", status, payload)
	}

	up, _ := payload["upload"].(map[string]any)
	headers, _ := up["headers"].(map[string]any)
	if got, _ := headers["x-amz-checksum-sha256"].(string); got != encoded {
		t.Errorf("upload.headers[x-amz-checksum-sha256] = %q, want %q", got, encoded)
	}

	url := uploadURL(t, payload)
	if got := put(t, url, body); got != http.StatusForbidden {
		t.Errorf("PUT without the checksum header = %d, want 403", got)
	}

	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("x-amz-checksum-sha256", encoded)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("PUT with the checksum header = %d, want 200", res.StatusCode)
	}
}

// takeDown issues the takedown, with a claim token when one is given.
func takeDown(t *testing.T, server *httptest.Server, token, slug, claimToken string) (int, map[string]any) {
	t.Helper()
	body, contentType := "", ""
	if claimToken != "" {
		body = fmt.Sprintf(`{"claim_token":%q}`, claimToken)
		contentType = "application/json"
	}
	return request(t, http.MethodDelete, server.URL+"/v1/artifacts/"+slug, token, contentType, body)
}

// readyArtifact declares, uploads and finalizes, so a test starts from the state
// a takedown actually meets in practice.
func readyArtifact(t *testing.T, server *httptest.Server, token, body string) map[string]any {
	t.Helper()
	payload := declare(t, server, token, "a.txt", body)
	if put(t, uploadURL(t, payload), body) != http.StatusOK {
		t.Fatal("upload failed")
	}
	if status, final := finalize(t, server, token, payload); status != http.StatusOK {
		t.Fatalf("finalize = %d %v", status, final)
	}
	return payload
}

// Takedown is the path someone reaches for when a secret was published by
// accident, so the bytes have to be gone at once — and what stays behind is a
// tombstone, because the link is already pasted somewhere and its reader
// deserves 410 rather than being sent hunting for a typo.
func TestTakedownRemovesTheBytesAndAnswersGoneEverywhere(t *testing.T) {
	server, _ := newClockedServer(t)
	const key = "krowk_sk_test"

	payload := readyArtifact(t, server, key, "a secret")
	slug, _ := payload["slug"].(string)
	fileURL, _ := payload["file_url"].(string)

	if status, body := takeDown(t, server, key, slug, ""); status != http.StatusNoContent {
		t.Fatalf("takedown = %d %v, want 204", status, body)
	}

	// The bytes go first, and they go for good: a takedown that could be undone
	// would leave a leaked secret leaked.
	if status, _ := request(t, http.MethodGet, fileURL, "", "", ""); status != http.StatusNotFound {
		t.Errorf("GET %s after takedown = %d, want 404", fileURL, status)
	}
	// The card stays and reports the takedown, without naming the file: a
	// takedown is somebody asking for it to be gone, and echoing the filename
	// back out of the page would undo half of that.
	card, _ := payload["url"].(string)
	status, page := requestText(t, card)
	if status != http.StatusGone {
		t.Errorf("GET %s after takedown = %d, want 410", card, status)
	}
	if strings.Contains(page, "a.txt") {
		t.Errorf("the taken-down card names the file:\n%s", page)
	}
	if status, body := request(t, http.MethodGet, server.URL+"/v1/artifacts/"+slug, key, "", ""); status != http.StatusGone || errorCode(body) != "taken_down" {
		t.Errorf("show after takedown = %d %v, want 410 taken_down", status, body)
	}
	// Finalizing has to report the takedown rather than its own idempotent
	// success: a taken-down artifact is normally a ready one, and answering 200
	// would hand back a url pointing at bytes that are no longer there.
	if status, body := finalize(t, server, key, payload); status != http.StatusGone || errorCode(body) != "taken_down" {
		t.Errorf("finalize after takedown = %d %v, want 410 taken_down", status, body)
	}

	status, created := request(t, http.MethodPost, server.URL+"/v1/runs", key, "application/json", `{"run":{}}`)
	if status != http.StatusCreated {
		t.Fatalf("create run = %d %v", status, created)
	}
	runSlug, _ := created["slug"].(string)
	status, body := request(t, http.MethodPut, server.URL+"/v1/artifacts/"+slug+"/run",
		key, "application/json", fmt.Sprintf(`{"run":%q}`, runSlug))
	if status != http.StatusGone || errorCode(body) != "taken_down" {
		t.Errorf("attach after takedown = %d %v, want 410 taken_down", status, body)
	}

	// A tombstone is not something the workspace still holds, so it answers for
	// its own slug and nowhere else.
	status, listing := request(t, http.MethodGet, server.URL+"/v1/artifacts", key, "", "")
	if status != http.StatusOK {
		t.Fatalf("list = %d %v", status, listing)
	}
	if artifacts, _ := listing["artifacts"].([]any); len(artifacts) != 0 {
		t.Errorf("listing still holds the tombstone: %v", artifacts)
	}

	// Idempotent, like finalizing: taking down what is already down is a success,
	// so a retry after a lost response is not a 404.
	if status, body := takeDown(t, server, key, slug, ""); status != http.StatusNoContent {
		t.Errorf("second takedown = %d %v, want 204", status, body)
	}
}

// For a keyless caller the claim token is the authority and the slug is not.
// Reading by slug alone is right — the bytes are public on the CDN regardless —
// but a slug travels in whatever the link was pasted into, so a takedown
// authorised by slug would let any reader of the paste destroy what they read.
func TestKeylessTakedownNeedsTheClaimTokenAndNotJustTheSlug(t *testing.T) {
	server, _ := newClockedServer(t)

	payload := readyArtifact(t, server, "", "the bytes")
	slug, _ := payload["slug"].(string)
	claimToken, _ := payload["claim_token"].(string)

	if status, body := takeDown(t, server, "", slug, ""); status != http.StatusBadRequest || errorCode(body) != "parameter_missing" {
		t.Errorf("takedown with no token = %d %v, want 400 parameter_missing", status, body)
	}
	if status, body := takeDown(t, server, "", slug, "krowk_claim_wrong"); status != http.StatusNotFound || errorCode(body) != "not_found" {
		t.Errorf("takedown with a wrong token = %d %v, want 404", status, body)
	}
	if status, _ := request(t, http.MethodGet, server.URL+"/v1/artifacts/"+slug, "", "", ""); status != http.StatusOK {
		t.Error("a refused takedown took the artifact down anyway")
	}

	if status, body := takeDown(t, server, "", slug, claimToken); status != http.StatusNoContent {
		t.Fatalf("takedown with the real token = %d %v, want 204", status, body)
	}
	if status, body := request(t, http.MethodGet, server.URL+"/v1/artifacts/"+slug, "", "", ""); status != http.StatusGone || errorCode(body) != "taken_down" {
		t.Errorf("show after takedown = %d %v, want 410 taken_down", status, body)
	}
}

// A key's authority is the workspace it acts in, so another tenant's slug reads
// as simply not existing rather than as forbidden — which would confirm it does.
func TestTakedownIsScopedToTheKeysWorkspace(t *testing.T) {
	server, _ := newClockedServer(t)

	payload := readyArtifact(t, server, "krowk_sk_mine", "the bytes")
	slug, _ := payload["slug"].(string)

	if status, body := takeDown(t, server, "krowk_sk_theirs", slug, ""); status != http.StatusNotFound {
		t.Errorf("takedown across workspaces = %d %v, want 404", status, body)
	}
	if status, _ := request(t, http.MethodGet, server.URL+"/v1/artifacts/"+slug, "krowk_sk_mine", "", ""); status != http.StatusOK {
		t.Error("another workspace's takedown went through")
	}
}

// Claiming spends the token and moves the artifact into a real workspace, so
// from then on the key that claimed it is what takes it down. A token that no
// longer speaks for anything must not keep speaking for this.
func TestSpentClaimTokenNoLongerTakesAnArtifactDown(t *testing.T) {
	server, _ := newClockedServer(t)
	const key = "krowk_sk_test"

	payload := readyArtifact(t, server, "", "the bytes")
	slug, _ := payload["slug"].(string)
	claimToken, _ := payload["claim_token"].(string)

	status, claimed := request(t, http.MethodPost, server.URL+"/v1/artifacts/"+slug+"/claim",
		key, "application/json", fmt.Sprintf(`{"claim_token":%q}`, claimToken))
	if status != http.StatusOK {
		t.Fatalf("claim = %d %v", status, claimed)
	}

	if status, body := takeDown(t, server, "", slug, claimToken); status != http.StatusNotFound {
		t.Errorf("takedown with a spent token = %d %v, want 404", status, body)
	}
	if status, body := takeDown(t, server, key, slug, ""); status != http.StatusNoContent {
		t.Errorf("takedown by the claiming key = %d %v, want 204", status, body)
	}
}

// Nothing claims an artifact back out of a takedown, and the registry says so as
// a 404 rather than a 410: it reaches the artifact through Artifact.claimable,
// which narrows to `live`, so a tombstone is not found rather than gone. Getting
// this wrong would have a client branch differently against --dev than against
// production, which is the whole failure a stand-in exists to prevent.
func TestATakenDownArtifactCannotBeClaimedBack(t *testing.T) {
	server, _ := newClockedServer(t)

	payload := readyArtifact(t, server, "", "the bytes")
	slug, _ := payload["slug"].(string)
	claimToken, _ := payload["claim_token"].(string)

	if status, body := takeDown(t, server, "", slug, claimToken); status != http.StatusNoContent {
		t.Fatalf("takedown = %d %v", status, body)
	}

	status, body := request(t, http.MethodPost, server.URL+"/v1/artifacts/"+slug+"/claim",
		"krowk_sk_test", "application/json", fmt.Sprintf(`{"claim_token":%q}`, claimToken))
	if status != http.StatusNotFound || errorCode(body) != "not_found" {
		t.Errorf("claim after takedown = %d %v, want 404 not_found", status, body)
	}
}

// An artifact can be both taken down and past its expiry, and the one somebody
// decided is the truer answer — so the order the two are reported in is load
// bearing rather than incidental.
func TestTakedownIsReportedAheadOfAnExpiry(t *testing.T) {
	server, clk := newClockedServer(t)

	payload := readyArtifact(t, server, "", "the bytes")
	slug, _ := payload["slug"].(string)
	claimToken, _ := payload["claim_token"].(string)

	if status, _ := takeDown(t, server, "", slug, claimToken); status != http.StatusNoContent {
		t.Fatal("takedown failed")
	}
	clk.advance(ephemeralLifetime + time.Minute)

	if status, body := request(t, http.MethodGet, server.URL+"/v1/artifacts/"+slug, "", "", ""); status != http.StatusGone || errorCode(body) != "taken_down" {
		t.Errorf("show = %d %v, want 410 taken_down rather than expired", status, body)
	}
}

// A takedown spends whatever upload URL is still outstanding. Without that, an
// artifact taken down before its bytes arrived would accept them afterwards and
// resurrect the public link under a tombstone.
func TestTakedownSpendsAnOutstandingUploadURL(t *testing.T) {
	server, _ := newClockedServer(t)
	const body = "the bytes"

	// Declared but deliberately not uploaded, so the presigned PUT is still live.
	payload := declare(t, server, "", "a.txt", body)
	slug, _ := payload["slug"].(string)
	claimToken, _ := payload["claim_token"].(string)
	url := uploadURL(t, payload)

	if status, got := takeDown(t, server, "", slug, claimToken); status != http.StatusNoContent {
		t.Fatalf("takedown = %d %v", status, got)
	}

	if status := put(t, url, body); status != http.StatusForbidden {
		t.Errorf("PUT after takedown = %d, want 403 — the URL is spent", status)
	}
	public, _ := payload["file_url"].(string)
	if status, _ := request(t, http.MethodGet, public, "", "", ""); status != http.StatusNotFound {
		t.Errorf("GET %s after takedown = %d, want 404", public, status)
	}
}

// A retry still succeeds after the artifact it claimed was taken down.
//
// The registry answers a retry from `Current.workspace.artifacts.find_by`, which
// is not scoped to `live` — so this is 200 with the tombstone, while a *fresh*
// claim on a tombstone is the 404 the test above pins. The two live one line
// apart here, and only the order between them keeps both right: moving the
// tombstone check ahead of the retry branch leaves every other test passing and
// turns this case into a 404, which is the same --dev-versus-production split
// the 404 was introduced to close.
func TestAClaimRetryStillSucceedsAfterTheArtifactWasTakenDown(t *testing.T) {
	server, _ := newClockedServer(t)
	const key = "krowk_sk_test"

	payload := readyArtifact(t, server, "", "the bytes")
	slug, _ := payload["slug"].(string)
	claimToken, _ := payload["claim_token"].(string)
	claim := func() (int, map[string]any) {
		return request(t, http.MethodPost, server.URL+"/v1/artifacts/"+slug+"/claim",
			key, "application/json", fmt.Sprintf(`{"claim_token":%q}`, claimToken))
	}

	if status, body := claim(); status != http.StatusOK {
		t.Fatalf("claim = %d %v", status, body)
	}
	if status, body := takeDown(t, server, key, slug, ""); status != http.StatusNoContent {
		t.Fatalf("takedown = %d %v", status, body)
	}

	status, body := claim()
	if status != http.StatusOK {
		t.Fatalf("claim retry after takedown = %d %v, want 200", status, body)
	}
	if got, _ := body["slug"].(string); got != slug {
		t.Errorf("retry answered with %q, want the artifact itself", got)
	}
}

func slugsOf(t *testing.T, payload map[string]any, key string) []string {
	t.Helper()
	rows, _ := payload[key].([]any)
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		record, _ := row.(map[string]any)
		slug, _ := record["slug"].(string)
		out = append(out, slug)
	}
	return out
}

// Runs page the way artifacts do — newest first, "older than this one" rather
// than an offset, so rows are neither skipped nor repeated when one is opened
// mid-listing.
func TestRunListingPagesNewestFirst(t *testing.T) {
	server, _ := newClockedServer(t)
	const key = "krowk_sk_test"

	var opened []string
	for range 3 {
		opened = append(opened, openRun(t, server, key))
	}
	newest, middle, oldest := opened[2], opened[1], opened[0]

	status, first := request(t, http.MethodGet, server.URL+"/v1/runs?limit=2", key, "", "")
	if status != http.StatusOK {
		t.Fatalf("list runs = %d %v", status, first)
	}
	if got := slugsOf(t, first, "runs"); !reflect.DeepEqual(got, []string{newest, middle}) {
		t.Errorf("first page = %v, want the two newest in order", got)
	}
	// next is present because the page came back full, and carries the cursor.
	if got, _ := first["next"].(string); got != middle {
		t.Errorf("next = %q, want %q", got, middle)
	}

	_, second := request(t, http.MethodGet, server.URL+"/v1/runs?limit=2&before="+middle, key, "", "")
	if got := slugsOf(t, second, "runs"); !reflect.DeepEqual(got, []string{oldest}) {
		t.Errorf("second page = %v, want the oldest alone", got)
	}
	if second["next"] != nil {
		t.Errorf("next = %v, want null on the last page", second["next"])
	}

	// Another key sees none of them.
	_, theirs := request(t, http.MethodGet, server.URL+"/v1/runs", "krowk_sk_theirs", "", "")
	if got := slugsOf(t, theirs, "runs"); len(got) != 0 {
		t.Errorf("another workspace's listing = %v, want empty", got)
	}
}

func TestShowRunIsScopedToTheKeysWorkspace(t *testing.T) {
	server, _ := newClockedServer(t)

	status, opened := request(t, http.MethodPost, server.URL+"/v1/runs", "krowk_sk_mine",
		"application/json", `{"run":{"metadata":{"repo":"acme/storefront"}}}`)
	if status != http.StatusCreated {
		t.Fatalf("open run = %d %v", status, opened)
	}
	slug, _ := opened["slug"].(string)

	status, payload := request(t, http.MethodGet, server.URL+"/v1/runs/"+slug, "krowk_sk_mine", "", "")
	if status != http.StatusOK {
		t.Fatalf("show run = %d %v", status, payload)
	}
	metadata, _ := payload["metadata"].(map[string]any)
	if metadata["repo"] != "acme/storefront" {
		t.Errorf("metadata = %v, want the run's own", payload["metadata"])
	}

	if status, _ := request(t, http.MethodGet, server.URL+"/v1/runs/"+slug, "krowk_sk_theirs", "", ""); status != http.StatusNotFound {
		t.Errorf("show across workspaces = %d, want 404", status)
	}
	if status, _ := request(t, http.MethodGet, server.URL+"/v1/runs/"+slug, "", "", ""); status != http.StatusUnauthorized {
		t.Errorf("show without a key = %d, want 401", status)
	}
}

// A run's artifacts are a collection of the run, not a filter on the workspace
// listing. The difference is the whole reason for the endpoint: an unknown run
// is a 404 from the run itself, where a filter would answer an empty page — and
// a caller cannot tell that apart from a run that produced nothing.
func TestRunArtifactsAreACollectionOfTheRunRatherThanAFilter(t *testing.T) {
	server, _ := newClockedServer(t)
	const key = "krowk_sk_test"

	mine := openRun(t, server, key)
	other := openRun(t, server, key)

	inRun := declare(t, server, key, "a.txt", "aa")
	slug, _ := inRun["slug"].(string)
	status, body := request(t, http.MethodPut, server.URL+"/v1/artifacts/"+slug+"/run",
		key, "application/json", fmt.Sprintf(`{"run":%q}`, mine))
	if status != http.StatusOK {
		t.Fatalf("attach = %d %v", status, body)
	}
	declare(t, server, key, "loose.txt", "bb") // in the workspace, in no run

	status, page := request(t, http.MethodGet, server.URL+"/v1/runs/"+mine+"/artifacts", key, "", "")
	if status != http.StatusOK {
		t.Fatalf("run artifacts = %d %v", status, page)
	}
	if got := slugsOf(t, page, "artifacts"); !reflect.DeepEqual(got, []string{slug}) {
		t.Errorf("run artifacts = %v, want only what the run made", got)
	}

	// A run that made nothing is an empty page, and it is answerable.
	_, empty := request(t, http.MethodGet, server.URL+"/v1/runs/"+other+"/artifacts", key, "", "")
	if got := slugsOf(t, empty, "artifacts"); len(got) != 0 {
		t.Errorf("empty run = %v, want no artifacts", got)
	}
	// A run that does not exist is not the same thing, and must not read as one.
	if status, _ := request(t, http.MethodGet, server.URL+"/v1/runs/run_nosuchrun0000000000/artifacts", key, "", ""); status != http.StatusNotFound {
		t.Errorf("unknown run = %d, want 404 rather than an empty page", status)
	}
	// Nor is another workspace's run, which reads as not existing rather than as
	// forbidden — which would confirm that it does.
	if status, _ := request(t, http.MethodGet, server.URL+"/v1/runs/"+mine+"/artifacts", "krowk_sk_theirs", "", ""); status != http.StatusNotFound {
		t.Errorf("another workspace's run = %d, want 404", status)
	}
}

// The `next` rule is the one part of the pagination contract a client cannot
// discover for itself, and it is deliberately not "next when rows were
// truncated": it is present whenever the page came back full, so a total that is
// an exact multiple of the limit costs one extra empty page rather than a count
// query on every listing. Rows == limit exactly is the only case that tells the
// two rules apart.
func TestAFullPageAlwaysCarriesACursorEvenWhenItIsTheLast(t *testing.T) {
	server, _ := newClockedServer(t)
	const key = "krowk_sk_test"

	first := openRun(t, server, key)
	second := openRun(t, server, key)

	status, page := request(t, http.MethodGet, server.URL+"/v1/runs?limit=2", key, "", "")
	if status != http.StatusOK {
		t.Fatalf("list = %d %v", status, page)
	}
	if got := slugsOf(t, page, "runs"); len(got) != 2 {
		t.Fatalf("page = %v, want both runs", got)
	}
	// Exactly as many rows as asked for, and nothing behind them — the cursor is
	// still handed over rather than withheld.
	next, _ := page["next"].(string)
	if next != first {
		t.Fatalf("next = %v, want %q even though this is the last page", page["next"], first)
	}

	_, beyond := request(t, http.MethodGet, server.URL+"/v1/runs?limit=2&before="+next, key, "", "")
	if got := slugsOf(t, beyond, "runs"); len(got) != 0 {
		t.Errorf("page after the cursor = %v, want it empty", got)
	}
	if beyond["next"] != nil {
		t.Errorf("next = %v, want null once the rows run out", beyond["next"])
	}
	_ = second
}

// The page size is the caller's to ask for and the registry's to bound. Asking
// for more than is served gets the most that is served — which is what was
// wanted — and anything that is not a number is not a request for one.
func TestPageSizeIsClampedRatherThanRefused(t *testing.T) {
	server, _ := newClockedServer(t)
	const key = "krowk_sk_test"

	for range maxPageSize + 2 {
		openRun(t, server, key)
	}

	for _, limit := range []string{"", "0", "-1", "abc", "999999999999999999999"} {
		url := server.URL + "/v1/runs"
		if limit != "" {
			url += "?limit=" + limit
		}
		status, page := request(t, http.MethodGet, url, key, "", "")
		if status != http.StatusOK {
			t.Errorf("limit=%q = %d %v, want 200", limit, status, page)
			continue
		}
		got := len(slugsOf(t, page, "runs"))
		// An unreadable or out-of-range limit is the default; a readable one is
		// clamped into the served range. Neither is ever unbounded.
		want := defaultPageSize
		switch limit {
		case "0", "-1":
			want = 1
		case "999999999999999999999":
			// Not refused for being unrepresentable: the registry parses it as a
			// bignum and clamps it like any other number, so this has to land on
			// the ceiling rather than quietly fall back to the default.
			want = maxPageSize
		}
		if got != want {
			t.Errorf("limit=%q served %d rows, want %d", limit, got, want)
		}
	}

	_, page := request(t, http.MethodGet, server.URL+"/v1/runs?limit=999", key, "", "")
	if got := len(slugsOf(t, page, "runs")); got != maxPageSize {
		t.Errorf("limit=999 served %d rows, want the %d ceiling", got, maxPageSize)
	}
}

// A run's artifact listing is scoped three ways, and each one is a separate way
// to serve the wrong rows: the cursor has to belong to this run, an unknown
// cursor is a 404 rather than a silent first page, and a tombstone is not
// something the run still holds.
func TestARunsArtifactListingIsScopedToTheRunAndToWhatItStillHolds(t *testing.T) {
	server, _ := newClockedServer(t)
	const key = "krowk_sk_test"

	mine := openRun(t, server, key)
	other := openRun(t, server, key)

	attach := func(runSlug, name string) string {
		payload := readyArtifact(t, server, key, "bytes for "+name)
		slug, _ := payload["slug"].(string)
		status, body := request(t, http.MethodPut, server.URL+"/v1/artifacts/"+slug+"/run",
			key, "application/json", fmt.Sprintf(`{"run":%q}`, runSlug))
		if status != http.StatusOK {
			t.Fatalf("attach %s = %d %v", name, status, body)
		}
		return slug
	}
	older := attach(mine, "older")
	newer := attach(mine, "newer")
	elsewhere := attach(other, "elsewhere")

	listing := func(query string) (int, map[string]any) {
		return request(t, http.MethodGet, server.URL+"/v1/runs/"+mine+"/artifacts"+query, key, "", "")
	}

	// A cursor from another run is not a cursor for this one.
	if status, _ := listing("?before=" + elsewhere); status != http.StatusNotFound {
		t.Errorf("cursor from another run = %d, want 404", status)
	}
	// Nor is one that names nothing at all — answering the first page instead
	// would silently ignore the caller's position.
	if status, _ := listing("?before=art_nosuchartifact00001"); status != http.StatusNotFound {
		t.Errorf("unknown cursor = %d, want 404", status)
	}
	// A cursor of this run's own pages within it.
	_, page := listing("?before=" + newer)
	if got := slugsOf(t, page, "artifacts"); !reflect.DeepEqual(got, []string{older}) {
		t.Errorf("page after the newest = %v, want the older one alone", got)
	}

	// A tombstone is not part of what the run still holds, exactly as it is not
	// part of what the workspace holds.
	if status, body := takeDown(t, server, key, older, ""); status != http.StatusNoContent {
		t.Fatalf("takedown = %d %v", status, body)
	}
	_, remaining := listing("")
	if got := slugsOf(t, remaining, "artifacts"); !reflect.DeepEqual(got, []string{newer}) {
		t.Errorf("run listing after a takedown = %v, want the tombstone gone", got)
	}
}

// The two 404s the registry tells apart, and the stand-in has to tell apart
// with it: an unknown slug is about what was asked for, an unknown path is
// about where it was asked. A client branching on the code gives one of them
// the wrong advice if they arrive spelled the same.
func TestUnroutedPathsAreNotTheSame404AsUnknownRecords(t *testing.T) {
	server, _ := newClockedServer(t)

	if status, payload := request(t, http.MethodGet, server.URL+"/v1/nothing-here",
		"krowk_sk_owner", "", ""); status != http.StatusNotFound || errorCode(payload) != "no_such_endpoint" {
		t.Errorf("unrouted path = %d %s, want 404 no_such_endpoint", status, errorCode(payload))
	}
	if status, payload := request(t, http.MethodGet, server.URL+"/v1/artifacts/art_nosuchartifact000",
		"krowk_sk_owner", "", ""); status != http.StatusNotFound || errorCode(payload) != "not_found" {
		t.Errorf("unknown slug = %d %s, want 404 not_found", status, errorCode(payload))
	}
}

// A known path asked for with a verb it does not serve is the same answer, and
// deliberately not a 405: it is what Go's mux does once a catch-all is
// registered, and what the Rails router the real registry runs does on its own.
func TestAVerbTheEndpointDoesNotServeIsNoSuchEndpoint(t *testing.T) {
	server, _ := newClockedServer(t)

	if status, payload := request(t, http.MethodPut, server.URL+"/v1/artifacts",
		"krowk_sk_owner", "application/json", "{}"); status != http.StatusNotFound ||
		errorCode(payload) != "no_such_endpoint" {
		t.Errorf("wrong verb = %d %s, want 404 no_such_endpoint", status, errorCode(payload))
	}
}

// A body the registry could not read has nothing to name a parameter from, so
// it is refused as the broken payload it is rather than as a field that is
// absent — which would send a client looking for one field in a body where
// every field is unreadable.
func TestABodyThatDoesNotParseIsRefusedAsOne(t *testing.T) {
	server, _ := newClockedServer(t)

	for name, body := range map[string]string{
		"truncated": `{"artifact": `,
		"not json":  `{not json at all`,
	} {
		status, payload := request(t, http.MethodPost, server.URL+"/v1/artifacts",
			"krowk_sk_owner", "application/json", body)
		if status != http.StatusBadRequest || errorCode(payload) != "bad_request" {
			t.Errorf("%s body = %d %s, want 400 bad_request", name, status, errorCode(payload))
		}
	}

	// An absent body is not an unreadable one: nothing was sent, so the missing
	// parameter is exactly what to name.
	if status, payload := request(t, http.MethodPost, server.URL+"/v1/artifacts",
		"krowk_sk_owner", "application/json", ""); status != http.StatusBadRequest ||
		errorCode(payload) != "parameter_missing" {
		t.Errorf("empty body = %d %s, want 400 parameter_missing", status, errorCode(payload))
	}
}

// The card page is what `url` now points at, and the whole reason it stopped
// pointing at the object: a paste destination fetches it, reads the OpenGraph
// tags and renders a preview. So the tags are the contract, and og:image has to
// name the bytes — an unfurler fetching og:image expects image bytes back and
// would otherwise get this page.
func TestCardPageCarriesTheOpenGraphTags(t *testing.T) {
	server, _ := newClockedServer(t)
	const key = "krowk_sk_test"

	payload := declareTyped(t, server, key, "shot.png", "image/png", "some bytes")
	// A declared artifact has no bytes yet. It renders as pending rather than as
	// an image that is not there: a card built on it would otherwise carry a
	// broken image for as long as the upload takes.
	card, _ := payload["url"].(string)
	status, page := requestText(t, card)
	if status != http.StatusOK || !strings.Contains(page, "pending") {
		t.Fatalf("pending card = %d\n%s", status, page)
	}
	if strings.Contains(page, `property="og:image"`) {
		t.Errorf("a pending card promises an image that is not there:\n%s", page)
	}

	if status, _ := request(t, http.MethodPut, uploadURL(t, payload), "", "image/png", "some bytes"); status != http.StatusOK {
		t.Fatal("upload failed")
	}
	if status, body := finalize(t, server, key, payload); status != http.StatusOK {
		t.Fatalf("finalize = %d %v", status, body)
	}

	fileURL, _ := payload["file_url"].(string)
	status, page = requestText(t, card)
	if status != http.StatusOK {
		t.Fatalf("card = %d\n%s", status, page)
	}
	for _, want := range []string{
		`<meta property="og:image" content="` + fileURL + `">`,
		`<meta property="og:title" content="shot.png">`,
		`<meta property="og:url" content="` + card + `">`,
		`<meta property="og:type"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("card is missing %s:\n%s", want, page)
		}
	}

	// A slug nobody ever issued is a 404 rather than a blank card.
	if status, _ := requestText(t, server.URL+"/a/art_nosuchartifact00000"); status != http.StatusNotFound {
		t.Errorf("unknown slug = %d, want 404", status)
	}
}

// Only an image gets an og:image. A log with one would have every card built
// from it carrying a broken thumbnail.
func TestCardPageOffersNoImageForANonImage(t *testing.T) {
	server, _ := newClockedServer(t)

	payload := declare(t, server, "krowk_sk_test", "build.log", "some bytes")
	card, _ := payload["url"].(string)
	_, page := requestText(t, card)
	if strings.Contains(page, "og:image") {
		t.Errorf("a log's card offers an image:\n%s", page)
	}
}

// Canon fixes a slug at a type prefix plus 24 random lowercase base36
// characters, because a slug becomes a DNS label at
// art-{slug}.krowkusercontent.com and labels are case-insensitive. Readers hold
// the stand-in to it: the website validates /^art_[a-z0-9]{24}$/ before it ever
// calls the registry, so a locally minted slug of any other shape is refused by
// a dev site before anyone finds out why.
func TestMintedSlugsHaveTheCanonicalShape(t *testing.T) {
	server, _ := newClockedServer(t)
	shape := regexp.MustCompile(`^(art|run|ws)_[a-z0-9]{24}$`)

	artifact, _ := declare(t, server, "krowk_sk_test", "shot.png", "some bytes")["slug"].(string)
	run := openRun(t, server, "krowk_sk_test")

	_, key := request(t, http.MethodGet, server.URL+"/v1/key", "krowk_sk_test", "", "")
	workspace, _ := key["workspace"].(string)

	for _, slug := range []string{artifact, run, workspace, anonymousWorkspace} {
		if !shape.MatchString(slug) {
			t.Errorf("slug %q is not prefix + 24 lowercase base36", slug)
		}
	}

	// Random, not sequential: two artifacts in a row must not share a slug.
	second, _ := declare(t, server, "krowk_sk_test", "shot.png", "some bytes")["slug"].(string)
	if second == artifact {
		t.Errorf("two artifacts share the slug %q", artifact)
	}
}

// postText posts nothing and reads the answer as text. The approval endpoints are
// what a browser form submits to, so they answer HTML rather than JSON.
func postText(t *testing.T, url string) (int, string) {
	t.Helper()
	res, err := http.Post(url, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, string(body)
}

// openLogin asks for a browser login and returns both halves of it.
func openLogin(t *testing.T, server *httptest.Server) (slug, code string) {
	t.Helper()
	status, payload := request(t, http.MethodPost, server.URL+"/v1/cli/authorizations", "", "", "")
	if status != http.StatusCreated {
		t.Fatalf("opening a login answered %d: %v", status, payload)
	}
	slug, _ = payload["slug"].(string)
	code, _ = payload["code"].(string)
	if slug == "" || code == "" {
		t.Fatalf("a login without both halves is not one: %v", payload)
	}
	return slug, code
}

// decide presses one of the two buttons on the approval page, which is what a
// person does in production and what a test does instead.
func decide(t *testing.T, server *httptest.Server, code, button string) (int, string) {
	t.Helper()
	return postText(t, server.URL+"/_approve/cli/authorizations/"+url.PathEscape(code)+"/"+button)
}

func pollLogin(t *testing.T, server *httptest.Server, slug string) (int, map[string]any) {
	t.Helper()
	return request(t, http.MethodGet, server.URL+"/v1/cli/authorizations/"+url.PathEscape(slug), "", "", "")
}

// A browser login hands the key over exactly once, and the key it hands over is
// the same one every other endpoint will recognise — otherwise logging in would
// succeed and the first upload would fail.
func TestABrowserLoginHandsTheKeyOverOnce(t *testing.T) {
	server, _ := newClockedServer(t)
	slug, code := openLogin(t, server)

	status, pending := pollLogin(t, server, slug)
	if status != http.StatusOK || pending["state"] != authorizationPending {
		t.Fatalf("a fresh login is %d %v, want a pending 200", status, pending)
	}
	if pending["token"] != nil {
		t.Fatalf("a login nobody approved handed over a key: %v", pending)
	}

	if status, body := decide(t, server, code, "approval"); status != http.StatusOK {
		t.Fatalf("approving answered %d: %s", status, body)
	}

	status, granted := pollLogin(t, server, slug)
	token, _ := granted["token"].(string)
	if status != http.StatusOK || !strings.HasPrefix(token, "krowk_sk_") {
		t.Fatalf("an approved login is %d %v, want a key", status, granted)
	}
	// The key the approval minted has to be the key the rest of the registry
	// knows, or `auth login` and the `auth verify` after it disagree.
	_, key := request(t, http.MethodGet, server.URL+"/v1/key", token, "", "")
	if key["key_id"] != granted["key_id"] || key["workspace"] != granted["workspace"] {
		t.Errorf("the login named %v/%v and the key endpoint names %v/%v",
			granted["key_id"], granted["workspace"], key["key_id"], key["workspace"])
	}

	// One shot: the plaintext left with the read above and no copy stayed behind.
	status, spent := pollLogin(t, server, slug)
	if status != http.StatusGone || errorCode(spent) != "spent" {
		t.Fatalf("a second poll is %d %v, want 410 spent", status, spent)
	}
}

// The code approves and nothing else. It travels through terminals, chat messages
// and people reading it aloud, so anything it could be exchanged for is a hole:
// the slug is what collects the key, and it never appears in a browser.
func TestTheApprovalCodeNeverYieldsTheKey(t *testing.T) {
	server, _ := newClockedServer(t)
	slug, code := openLogin(t, server)

	if status, body := pollLogin(t, server, code); status != http.StatusNotFound {
		t.Errorf("polling by code answered %d %v, want 404 — the code is not the slug", status, body)
	}

	status, answer := decide(t, server, code, "approval")
	if status != http.StatusOK {
		t.Fatalf("approving answered %d: %s", status, answer)
	}
	// Approving is what mints the key, and the person approving is not who
	// collects it.
	if strings.Contains(answer, "krowk_sk_") {
		t.Errorf("the approval answered with the key itself:\n%s", answer)
	}
	if _, granted := pollLogin(t, server, slug); granted["token"] == nil {
		t.Errorf("the slug did not collect what the code approved: %v", granted)
	}
}

// Denied is an answer, and it keeps being one until the window closes: a CLI that
// went on polling after someone said no would sit there until it lapsed.
func TestADeniedBrowserLoginKeepsSayingSo(t *testing.T) {
	server, _ := newClockedServer(t)
	slug, code := openLogin(t, server)

	if status, body := decide(t, server, code, "denial"); status != http.StatusOK {
		t.Fatalf("denying answered %d: %s", status, body)
	}
	for range 2 {
		status, denied := pollLogin(t, server, slug)
		if status != http.StatusOK || denied["state"] != authorizationDenied {
			t.Fatalf("a denied login is %d %v, want a denied 200", status, denied)
		}
		if denied["token"] != nil {
			t.Fatalf("a denied login handed over a key: %v", denied)
		}
	}
	// And it cannot be talked round afterwards.
	if status, _ := decide(t, server, code, "approval"); status != http.StatusConflict {
		t.Errorf("approving a denied login answered %d, want 409", status)
	}
}

// A lapsed login is gone the way a lapsed artifact is: 410, so a client can tell
// "this existed and the window closed" from "no such login".
func TestABrowserLoginExpires(t *testing.T) {
	server, c := newClockedServer(t)
	slug, code := openLogin(t, server)

	c.advance(cliAuthorizationLifetime + time.Second)

	status, gone := pollLogin(t, server, slug)
	if status != http.StatusGone || errorCode(gone) != "expired" {
		t.Fatalf("a lapsed login is %d %v, want 410 expired", status, gone)
	}
	// Expiry beats approval, rather than approval reviving the window.
	if status, _ := decide(t, server, code, "approval"); status != http.StatusGone {
		t.Errorf("approving a lapsed login answered %d, want 410", status)
	}
}

// The approval page refuses a code that matches nothing, rather than offering
// buttons that would answer 404 once pressed.
func TestTheApprovalPageRefusesACodeThatMatchesNothing(t *testing.T) {
	server, _ := newClockedServer(t)
	_, code := openLogin(t, server)

	status, shown := requestText(t, server.URL+"/_approve/cli/authorizations/new?code="+code)
	if status != http.StatusOK || !strings.Contains(shown, code) {
		t.Fatalf("the page for a live code is %d and does not show it:\n%s", status, shown)
	}

	for _, unknown := range []string{"", "ZZZZ-ZZZZ"} {
		if status, _ := requestText(t,
			server.URL+"/_approve/cli/authorizations/new?code="+unknown); status != http.StatusNotFound {
			t.Errorf("the page for code %q answered %d, want 404", unknown, status)
		}
	}
}

// The code is read off one screen and typed into another, or read out loud, so
// its alphabet leaves out the two pairs that get confused. 32 characters also
// divides 256, which is what keeps the choice per byte unbiased.
func TestApprovalCodesAvoidConfusableCharacters(t *testing.T) {
	shape := regexp.MustCompile(`^[2-9A-HJ-NP-Z]{4}-[2-9A-HJ-NP-Z]{4}$`)
	seen := map[string]bool{}
	for range 200 {
		code := generateCode()
		if !shape.MatchString(code) {
			t.Fatalf("code %q is not two groups of four from the unambiguous alphabet", code)
		}
		seen[code] = true
	}
	if len(seen) < 190 {
		t.Errorf("200 codes produced only %d distinct ones", len(seen))
	}
	if strings.ContainsAny(codeAlphabet, "01OI") {
		t.Errorf("the code alphabet %q keeps a confusable character", codeAlphabet)
	}
}

// A key that was collected and then sat past its window was still collected. The
// CLI's advice differs between the two — "this lapsed before it was approved" is a
// lie about a key that is on somebody's disk — so spent has to outrank expiry.
func TestASpentBrowserLoginKeepsSayingSpentAfterItLapses(t *testing.T) {
	server, c := newClockedServer(t)
	slug, code := openLogin(t, server)

	if status, body := decide(t, server, code, "approval"); status != http.StatusOK {
		t.Fatalf("approving answered %d: %s", status, body)
	}
	if _, granted := pollLogin(t, server, slug); granted["token"] == nil {
		t.Fatalf("the key was never collected: %v", granted)
	}

	c.advance(cliAuthorizationLifetime + time.Second)

	status, gone := pollLogin(t, server, slug)
	if status != http.StatusGone || errorCode(gone) != "spent" {
		t.Fatalf("a collected login that then lapsed is %d %v, want 410 spent", status, gone)
	}
}

// One-shot means delivered once, not destroyed once. If the response never leaves,
// nobody received the key — and the laptop-wifi case the CLI's poll loop is
// written to survive must not be the case that makes a login unrecoverable.
func TestABrowserLoginKeepsItsKeyWhenTheResponseNeverLands(t *testing.T) {
	c := &clock{at: time.Now()}
	handler := HandlerWithClock(0, "", c.now)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	slug, code := openLogin(t, server)
	if status, body := decide(t, server, code, "approval"); status != http.StatusOK {
		t.Fatalf("approving answered %d: %s", status, body)
	}

	// A writer that refuses the body is the one seam that stands in for a
	// connection dropping between the status line and the payload.
	handler.ServeHTTP(brokenWriter{header: http.Header{}}, httptest.NewRequest(
		http.MethodGet, "/v1/cli/authorizations/"+url.PathEscape(slug), nil))

	status, granted := pollLogin(t, server, slug)
	token, _ := granted["token"].(string)
	if status != http.StatusOK || !strings.HasPrefix(token, "krowk_sk_") {
		t.Fatalf("the poll after a lost response is %d %v, want the key still collectable", status, granted)
	}
	// And it is one-shot again from there.
	if status, spent := pollLogin(t, server, slug); status != http.StatusGone || errorCode(spent) != "spent" {
		t.Fatalf("the poll after a delivered one is %d %v, want 410 spent", status, spent)
	}
}

// brokenWriter accepts the status and refuses the body, which is what a dropped
// connection looks like to a handler.
type brokenWriter struct{ header http.Header }

func (b brokenWriter) Header() http.Header       { return b.header }
func (b brokenWriter) WriteHeader(int)           {}
func (b brokenWriter) Write([]byte) (int, error) { return 0, errors.New("connection reset") }

// Lapsed logins are swept, or a stand-in registry left up all week accumulates
// every login it ever answered. The grace period is what keeps `410 expired`
// meaningful: reaped at the window's edge, a client that polled a second late
// would be told no such login rather than that the window closed.
func TestLapsedBrowserLoginsAreSweptButNotImmediately(t *testing.T) {
	server, c := newClockedServer(t)
	justLapsed, _ := openLogin(t, server)

	c.advance(cliAuthorizationLifetime + time.Minute)
	// Opening another is what sweeps, since it is the only moment the set grows.
	openLogin(t, server)

	if status, body := pollLogin(t, server, justLapsed); status != http.StatusGone ||
		errorCode(body) != "expired" {
		t.Errorf("a login inside the grace period is %d %v, want 410 expired", status, body)
	}

	c.advance(cliAuthorizationGrace)
	openLogin(t, server)

	if status, body := pollLogin(t, server, justLapsed); status != http.StatusNotFound {
		t.Errorf("a login past the grace period is %d %v, want it swept", status, body)
	}
}

// pasteOf digs out the paste envelope the registry computes for an artifact.
func pasteOf(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	paste, ok := payload["paste"].(map[string]any)
	if !ok {
		t.Fatalf("no paste envelope in %v", payload)
	}
	return paste
}

// Every krowk reference has the same silhouette, and the registry is what gives
// it one: an image embeds its bytes, clicks through to the card, and says
// underneath what it shows and where it goes.
func TestPasteBlockIsBuiltFromTheArtifactsOwnCaption(t *testing.T) {
	server, _ := newClockedServer(t)

	status, payload := request(t, http.MethodPost, server.URL+"/v1/artifacts", "hunter2", "application/json",
		`{"artifact":{"filename":"shot.png","content_type":"image/png","byte_size":4,
		  "metadata":{"krowk.caption":"Button update before"}}}`)
	if status != http.StatusCreated {
		t.Fatalf("declare = %d %v", status, payload)
	}

	paste := pasteOf(t, payload)
	url, _ := payload["url"].(string)
	fileURL, _ := payload["file_url"].(string)
	want := "[![Button update before](" + fileURL + ")](" + url + ")\n" +
		"Button update before · [View preview ↗](" + url + ")"
	if paste["markdown"] != want {
		t.Errorf("paste.markdown =\n%v\nwant\n%v", paste["markdown"], want)
	}
	// The other form is the bare card link, and it is always present: a
	// consumer picks by destination rather than by what it was sent.
	if paste["url"] != url {
		t.Errorf("paste.url = %v, want the card page %q", paste["url"], url)
	}
}

// No caption is the ordinary case, and the filename is the honest stand-in.
// Anything that would end or nest a link label leaves here escaped, or the
// block breaks wherever it is pasted.
func TestPasteBlockFallsBackToTheEscapedFilename(t *testing.T) {
	server, _ := newClockedServer(t)

	payload := declareTyped(t, server, "hunter2", "frame[0].png", "image/png", "bytes")

	paste := pasteOf(t, payload)
	markdown, _ := paste["markdown"].(string)
	if !strings.Contains(markdown, `[![frame\[0\].png](`) {
		t.Errorf("paste.markdown = %q, want the filename escaped in the label", markdown)
	}
}

// A log or a diff has nothing to embed, so the block is the same shape minus
// the image: the caption carries the line on its own, and is bolded because it
// is now the whole of it.
func TestPasteBlockForANonImageIsTheSameShapeWithoutTheImage(t *testing.T) {
	server, _ := newClockedServer(t)

	payload := declare(t, server, "hunter2", "deploy.log", "build output")

	paste := pasteOf(t, payload)
	url, _ := payload["url"].(string)
	want := "**deploy.log** · [View preview ↗](" + url + ")"
	if paste["markdown"] != want {
		t.Errorf("paste.markdown = %v, want %v", paste["markdown"], want)
	}
}

// An unclaimed artifact is on a clock, and a block that does not say so invites
// an inline image into a pull request comment that will outlive it.
func TestPasteBlockSaysWhenAnUnclaimedArtifactGoes(t *testing.T) {
	server, _ := newClockedServer(t)

	payload := declareTyped(t, server, "", "shot.png", "image/png", "bytes")

	paste := pasteOf(t, payload)
	markdown, _ := paste["markdown"].(string)
	if !strings.Contains(markdown, " · expires ") {
		t.Errorf("paste.markdown = %q, want the expiry named", markdown)
	}
}

// The destination table is the registry's, and it is served with every
// artifact: that is what lets a tool prove out without a client release.
func TestPasteCarriesTheDestinationTable(t *testing.T) {
	server, _ := newClockedServer(t)

	payload := declare(t, server, "hunter2", "deploy.log", "build output")

	destinations, ok := pasteOf(t, payload)["destinations"].(map[string]any)
	if !ok {
		t.Fatalf("no destinations table in %v", payload["paste"])
	}
	for destination, want := range map[string]string{
		"github": "markdown", "linear": "markdown",
		"slack": "url", "asana": "url",
		"_default": "markdown",
	} {
		if destinations[destination] != want {
			t.Errorf("destinations[%q] = %v, want %q", destination, destinations[destination], want)
		}
	}
}
