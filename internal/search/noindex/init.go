package noindex

import (
	"github.com/OpenListTeam/OpenList/v4/internal/search/searcher"
)

var config = searcher.Config{
	Name:       "no_index",
	AutoUpdate: false,
}

func init() {
	searcher.RegisterSearcher(config, func() (searcher.Searcher, error) {
		return &NoIndex{}, nil
	})
}
