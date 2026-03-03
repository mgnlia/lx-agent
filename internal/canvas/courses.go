package canvas

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

func (c *Client) GetCourses(ctx context.Context) ([]Course, error) {
	params := url.Values{
		"enrollment_state": {"active"},
		"per_page":         {"100"},
	}

	raw, err := c.getPaginated(ctx, "/courses", params)
	if err != nil {
		return nil, err
	}

	var courses []Course
	for _, r := range raw {
		var course Course
		if err := json.Unmarshal(r, &course); err != nil {
			continue
		}
		courses = append(courses, course)
	}
	return courses, nil
}

func (c *Client) GetCourse(ctx context.Context, courseID int) (*Course, error) {
	var course Course
	err := c.getAll(ctx, fmt.Sprintf("/courses/%d", courseID), nil, &course)
	return &course, err
}
