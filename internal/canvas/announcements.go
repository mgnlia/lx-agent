package canvas

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

func (c *Client) GetAnnouncements(ctx context.Context, courseIDs []int) ([]Announcement, error) {
	params := url.Values{
		"per_page":    {"50"},
		"latest_only": {"false"},
	}
	for _, id := range courseIDs {
		params.Add("context_codes[]", fmt.Sprintf("course_%d", id))
	}

	raw, err := c.getPaginated(ctx, "/announcements", params)
	if err != nil {
		return nil, err
	}

	var announcements []Announcement
	for _, r := range raw {
		var a Announcement
		if err := json.Unmarshal(r, &a); err != nil {
			continue
		}
		announcements = append(announcements, a)
	}
	return announcements, nil
}
