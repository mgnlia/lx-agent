package canvas

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

func (c *Client) GetFiles(ctx context.Context, courseID int) ([]File, error) {
	params := url.Values{
		"sort":     {"updated_at"},
		"order":    {"desc"},
		"per_page": {"50"},
	}

	raw, err := c.getPaginated(ctx, fmt.Sprintf("/courses/%d/files", courseID), params)
	if err != nil {
		return nil, err
	}

	var files []File
	for _, r := range raw {
		var f File
		if err := json.Unmarshal(r, &f); err != nil {
			continue
		}
		files = append(files, f)
	}
	return files, nil
}

func (c *Client) GetModules(ctx context.Context, courseID int) ([]Module, error) {
	params := url.Values{
		"include[]": {"items"},
		"per_page":  {"50"},
	}

	raw, err := c.getPaginated(ctx, fmt.Sprintf("/courses/%d/modules", courseID), params)
	if err != nil {
		return nil, err
	}

	var modules []Module
	for _, r := range raw {
		var m Module
		if err := json.Unmarshal(r, &m); err != nil {
			continue
		}
		modules = append(modules, m)
	}
	return modules, nil
}
