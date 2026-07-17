package memory

// ProjectScaffoldService owns layout-v2 project memory repo scaffold workflows.
// UI handlers use it to inspect and create missing canonical files without
// duplicating the sequence of layout discovery followed by filesystem writes.
type ProjectScaffoldService struct{}

// Missing returns the canonical layout entries absent from root.
func (ProjectScaffoldService) Missing(root string, global bool) ([]LayoutItem, error) {
	return MissingProjectRepoItems(root, global)
}

// CreateMissing creates any absent canonical layout entries and returns the
// number of entries created. A complete repo returns zero without writing.
func (ProjectScaffoldService) CreateMissing(root string, global bool) (int, error) {
	missing, err := MissingProjectRepoItems(root, global)
	if err != nil {
		return 0, err
	}
	if len(missing) == 0 {
		return 0, nil
	}
	if err := CreateMissing(root, missing); err != nil {
		return 0, err
	}
	return len(missing), nil
}
