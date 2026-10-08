package check

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// el encodes one EBML element with a 1-byte (or 2-byte) size.
func el(id uint32, payload ...[]byte) []byte {
	body := bytes.Join(payload, nil)
	var out []byte
	for _, sh := range []uint{24, 16, 8, 0} {
		if b := byte(id >> sh); b != 0 || len(out) > 0 {
			out = append(out, b)
		}
	}
	if len(body) < 127 {
		out = append(out, 0x80|byte(len(body)))
	} else {
		out = append(out, 0x40|byte(len(body)>>8), byte(len(body)))
	}
	return append(out, body...)
}

func trackEntry(num, typ int, codec, lang string) []byte {
	parts := [][]byte{el(idTrackNum, []byte{byte(num)}), el(idTrackType, []byte{byte(typ)}), el(idCodecID, []byte(codec))}
	if lang != "" {
		parts = append(parts, el(idLanguage, []byte(lang)))
	}
	return el(idTrackEnt, parts...)
}

// syntheticMKV is the head of a Matroska file: EBML header, an unknown-size
// Segment, a SeekHead and Info to skip, the Tracks, then a Cluster.
func syntheticMKV() []byte {
	header := el(idEBML, el(0x4282, []byte("matroska")))
	tracks := el(idTracks,
		trackEntry(1, 1, "V_MPEGH/ISO/HEVC", "und"),
		trackEntry(2, 2, "A_FLAC", "jpn"),
		trackEntry(3, TrackSubtitle, "S_TEXT/ASS", "chi"),
		trackEntry(4, TrackSubtitle, "S_HDMV/PGS", ""),
	)
	segChildren := bytes.Join([][]byte{el(0x114D9B74, make([]byte, 40)), el(0x1549A966, make([]byte, 200)), tracks, el(idCluster, make([]byte, 10))}, nil)
	seg := append([]byte{0x18, 0x53, 0x80, 0x67, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}, segChildren...) // unknown size
	return append(header, seg...)
}

func TestParseMKVTracks(t *testing.T) {
	ts, err := ParseMKVTracks(syntheticMKV())
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 4 {
		t.Fatalf("got %d tracks: %+v", len(ts), ts)
	}
	subs := SubtitleTracks(ts)
	if len(subs) != 2 || subs[0].Codec != "S_TEXT/ASS" || subs[0].Lang != "chi" || subs[1].Codec != "S_HDMV/PGS" || subs[1].Lang != "eng" {
		t.Errorf("subtitle tracks: %+v", subs)
	}
	if ts[1].Type != 2 || ts[1].Lang != "jpn" || ts[1].Number != 2 {
		t.Errorf("audio track: %+v", ts[1])
	}
}

func TestParseMKVTracksCutOff(t *testing.T) {
	full := syntheticMKV()
	// cut inside the last track entry: the complete ones still count
	i := bytes.Index(full, []byte("S_HDMV/PGS"))
	ts, err := ParseMKVTracks(full[:i+3])
	if err != nil || len(ts) != 3 {
		t.Errorf("cut inside the 4th entry: %d tracks, err=%v", len(ts), err)
	}
	// cut before Tracks: there is nothing to report
	j := bytes.Index(full, []byte{0x16, 0x54, 0xAE, 0x6B})
	if _, err := ParseMKVTracks(full[:j-5]); !errors.Is(err, ErrNoTracks) {
		t.Errorf("cut before Tracks: %v", err)
	}
	if _, err := ParseMKVTracks([]byte("RIFF....AVI ")); err == nil {
		t.Error("not a Matroska file must fail")
	}
	// a Cluster before any Tracks ends the search
	hdr := el(idEBML, el(0x4282, []byte("matroska")))
	seg := append([]byte{0x18, 0x53, 0x80, 0x67, 0xFF}, el(idCluster, make([]byte, 5))...)
	if _, err := ParseMKVTracks(append(hdr, seg...)); err == nil {
		t.Error("cluster first must fail")
	}
}

func TestRemoteProberReadsOneRange(t *testing.T) {
	mkv := syntheticMKV()
	var rangeHdr, authOnStorage string
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHdr, authOnStorage = r.Header.Get("Range"), r.Header.Get("Authorization")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(mkv)
	}))
	defer storage.Close()
	api := apiFunc(func(method, path string, body, out any) error {
		if path != "/api/fs/get" || body.(map[string]any)["path"] != "/lib/x.mkv" {
			t.Errorf("unexpected call %s %s %v", method, path, body)
		}
		*(out.(*struct {
			RawURL string `json:"raw_url"`
		})) = struct {
			RawURL string `json:"raw_url"`
		}{storage.URL + "/file"}
		return nil
	})
	p := RemoteProber{API: api, Base: "http://op.example:5244", Token: "secret"}
	ts, err := p.Probe(t.Context(), "/lib/x.mkv")
	if err != nil || len(ts) != 4 {
		t.Fatalf("tracks=%d err=%v", len(ts), err)
	}
	if rangeHdr != "bytes=0-"+strconv.Itoa(ProbeBytes-1) {
		t.Errorf("Range = %q", rangeHdr)
	}
	if authOnStorage != "" {
		t.Error("the server token must not go to the storage host")
	}
}

type apiFunc func(method, path string, body, out any) error

func (f apiFunc) Call(method, path string, body, out any) error { return f(method, path, body, out) }
func (f apiFunc) Upload(string, string, string, []byte, any) error {
	return errors.New("not used")
}
