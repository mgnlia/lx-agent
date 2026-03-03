package canvas

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

func (c *Client) GetAssignments(ctx context.Context, courseID int) ([]Assignment, error) {
	params := url.Values{
		"order_by": {"due_at"},
		"per_page": {"100"},
	}

	raw, err := c.getPaginated(ctx, fmt.Sprintf("/courses/%d/assignments", courseID), params)
	if err != nil {
		return nil, err
	}

	var assignments []Assignment
	for _, r := range raw {
		var a Assignment
		if err := json.Unmarshal(r, &a); err != nil {
			continue
		}
		a.CourseID = courseID
		assignments = append(assignments, a)
	}
	return assignments, nil
}
