package identify

import (
	"reflect"
	"testing"
)

func TestContentNameReadsTheLatinTitleAndYear(t *testing.T) {
	for _, c := range []struct {
		name   string
		isFile bool
		want   ContentName
		ok     bool
	}{
		{"One.Flew.Over.the.Cuckoos.Nest.1975.(Ultimate.Collectors.Edition).REMUX.1080p.Blu-ray.VC-1.DD.2.0-LEGi0N.mkv", true, ContentName{Name: "One Flew Over the Cuckoos Nest", Year: 1975}, true},
		{"【高清影视之家发布 www.WHATMV.com】回忆三部曲[中文字幕].Memories.1995.1080p.BluRay.Remux.AVC.DTS-HD.MA.5.1-QuickIO", false, ContentName{Name: "Memories", Year: 1995}, true},
		{"Love.Death.&.Robots.S01", false, ContentName{Name: "Love Death & Robots", Year: 0}, true},
		{"STAND ALONE COMPLEX", false, ContentName{Name: "STAND ALONE COMPLEX", Year: 0}, true},
		{"TV高压中字", false, ContentName{}, false},
		{"old", false, ContentName{}, false},
	} {
		got, ok := contentName(c.name, c.isFile)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("%q: %+v %v, want %+v %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestResultCarriesTheNamesFoundInside(t *testing.T) {
	r := Classify("飞跃疯人院", true, one("One.Flew.Over.the.Cuckoos.Nest.1975.REMUX.mkv"))
	want := []ContentName{{Name: "One Flew Over the Cuckoos Nest", Year: 1975}}
	if !reflect.DeepEqual(r.Content, want) {
		t.Fatalf("content %+v", r.Content)
	}
	r = Classify("攻壳机动队TV合集", true, dirs("S.A.C. 2nd GIG", "STAND ALONE COMPLEX"))
	if len(r.Content) == 0 || r.Content[len(r.Content)-1].Name != "STAND ALONE COMPLEX" {
		t.Fatalf("content %+v", r.Content)
	}
}
