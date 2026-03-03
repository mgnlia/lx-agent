package main

import (
	"testing"
	"time"

	"github.com/mgnlia/lx-agent/internal/canvas"
)

func TestDefaultSemesterCoursesForKey_ExpandsToSameTerm(t *testing.T) {
	courses := []canvas.Course{
		{ID: 1, Name: "2026-1 데이터베이스", EnrollmentTermID: 101},
		{ID: 2, Name: "Database Systems", EnrollmentTermID: 101},
		{ID: 3, Name: "2025-2 Older Course", EnrollmentTermID: 99},
	}

	got := defaultSemesterCoursesForKey(courses, "2026-1")
	if len(got) != 2 {
		t.Fatalf("expected 2 courses, got %d", len(got))
	}
	if got[0].ID != 1 || got[1].ID != 2 {
		t.Fatalf("expected course IDs [1,2], got [%d,%d]", got[0].ID, got[1].ID)
	}
}

func TestDefaultSemesterCoursesForKey_NoSemesterMatchReturnsAll(t *testing.T) {
	courses := []canvas.Course{
		{ID: 10, Name: "Machine Learning", EnrollmentTermID: 200},
		{ID: 11, Name: "Operating Systems", EnrollmentTermID: 200},
	}

	got := defaultSemesterCoursesForKey(courses, "2099-1")
	if len(got) != len(courses) {
		t.Fatalf("expected %d courses, got %d", len(courses), len(got))
	}
}

func TestFilterCoursesBySemesterWindow_UsesDateOverlap(t *testing.T) {
	kst := time.FixedZone("KST", 9*3600)
	now := time.Date(2026, time.March, 3, 9, 0, 0, 0, kst)

	koreanStart := time.Date(2026, time.February, 20, 9, 0, 0, 0, kst)
	englishStart := time.Date(2026, time.March, 1, 9, 0, 0, 0, kst)
	oldStart := time.Date(2025, time.September, 1, 9, 0, 0, 0, kst)
	oldEnd := time.Date(2025, time.December, 20, 9, 0, 0, 0, kst)

	courses := []canvas.Course{
		{ID: 1, Name: "2026-1 데이터베이스", EnrollmentTermID: 101, StartAt: &koreanStart},
		{ID: 2, Name: "Computer Architecture", EnrollmentTermID: 202, StartAt: &englishStart},
		{ID: 3, Name: "2025-2 Older Course", EnrollmentTermID: 99, StartAt: &oldStart, EndAt: &oldEnd},
	}

	got := filterCoursesBySemesterWindow(courses, now)
	if len(got) != 2 {
		t.Fatalf("expected 2 courses in semester window, got %d", len(got))
	}
	if got[0].ID != 1 || got[1].ID != 2 {
		t.Fatalf("expected course IDs [1,2], got [%d,%d]", got[0].ID, got[1].ID)
	}
}
