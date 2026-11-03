package model

import "testing"

func TestVersionsJSON(t *testing.T) {
	if VersionsJSON([]string{"a.mkv"}) != "" {
		t.Fatal("one file is not a set of versions")
	}
	j := VersionsJSON([]string{"Flipped.2010.mkv", "av1_recode_Flipped.2010.mkv"})
	vs := ItemVersions(j)
	if len(vs) != 2 || vs[0].FileName != "Flipped.2010.mkv" || vs[0].Index != 1 || vs[1].Index != 2 || !vs[0].Version || vs[0].Title != "Flipped.2010" {
		t.Fatalf("%s -> %+v", j, vs)
	}
	// episodes are not versions
	eps := EpisodesJSON([]MediaEpisode{{FileName: "a.S01E01.mkv", Season: 1, Episode: 1}, {FileName: "a.S01E02.mkv", Season: 1, Episode: 2}})
	if ItemVersions(eps) != nil || ItemVersions("") != nil || ItemVersions("not json") != nil {
		t.Fatal("episodes read as versions")
	}
}
