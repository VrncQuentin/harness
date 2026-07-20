package project

import (
	"errors"
	"testing"
)

func TestValidateSlug(t *testing.T) {
	tests := []struct {
		name string
		slug string
		want error
	}{
		{"simple", "dt", nil},
		{"dashed", "local-agent-1", nil},
		{"empty", "", ErrInvalidSlug},
		{"uppercase", "DT", ErrInvalidSlug},
		{"underscore", "local_agent", ErrInvalidSlug},
		{"double dash", "local--agent", ErrInvalidSlug},
		{"space padded", " global ", ErrInvalidSlug},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSlug(tc.slug)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("ValidateSlug(%q): %v", tc.slug, err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("ValidateSlug(%q): errors.Is(%v) = false, err=%v", tc.slug, tc.want, err)
			}
		})
	}
}

func TestValidateCreatableSlugRejectsGlobal(t *testing.T) {
	if err := ValidateCreatableSlug(GlobalSlug); !errors.Is(err, ErrReservedSlug) {
		t.Fatalf("ValidateCreatableSlug(global): errors.Is(ErrReservedSlug)=false, err=%v", err)
	}
}

func TestValidateCreatableSlugRejectsGlobalWithSpaces(t *testing.T) {
	if err := ValidateCreatableSlug(" global "); !errors.Is(err, ErrReservedSlug) {
		t.Fatalf("ValidateCreatableSlug(\" global \"): errors.Is(ErrReservedSlug)=false, err=%v", err)
	}
}

func TestValidateDirectoryPath(t *testing.T) {
	if err := ValidateDirectoryPath(t.TempDir()); err != nil {
		t.Fatalf("ValidateDirectoryPath(temp dir): %v", err)
	}
	if err := ValidateDirectoryPath(`C:\tmp\repo`); err != nil {
		t.Fatalf("ValidateDirectoryPath(windows abs): %v", err)
	}
	if err := ValidateDirectoryPath("relative"); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("ValidateDirectoryPath(relative): errors.Is(ErrInvalidPath)=false, err=%v", err)
	}
}

func TestValidateMemoryRepoPath(t *testing.T) {
	if err := ValidateMemoryRepoPath(t.TempDir()); err != nil {
		t.Fatalf("ValidateMemoryRepoPath(temp dir): %v", err)
	}
	if err := ValidateMemoryRepoPath(`C:/tmp/repo`); err != nil {
		t.Fatalf("ValidateMemoryRepoPath(windows abs): %v", err)
	}
	if err := ValidateMemoryRepoPath("relative"); !errors.Is(err, ErrMemoryRepo) {
		t.Fatalf("ValidateMemoryRepoPath(relative): errors.Is(ErrMemoryRepo)=false, err=%v", err)
	}
}
