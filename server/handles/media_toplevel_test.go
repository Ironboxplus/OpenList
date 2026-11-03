package handles

import (
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

func TestKindFilterListsFlat(t *testing.T) {
	v := model.MediaTypeVideo
	if !listsTopLevel(v, "", "", "", "", nil) {
		t.Fatal("the unfiltered video view must list top-level works only")
	}
	for name, got := range map[string]bool{
		"kind":    listsTopLevel(v, "", "", "", "", []string{"movie"}),
		"flat":    listsTopLevel(v, "", "", "", "1", nil),
		"keyword": listsTopLevel(v, "", "x", "", "", nil),
		"path":    listsTopLevel(v, "", "", "/a", "", nil),
		"folder":  listsTopLevel(v, "/a", "", "", "", nil),
		"music":   listsTopLevel(model.MediaTypeMusic, "", "", "", "", nil),
	} {
		if got {
			t.Errorf("%s: listed top-level only", name)
		}
	}
}
