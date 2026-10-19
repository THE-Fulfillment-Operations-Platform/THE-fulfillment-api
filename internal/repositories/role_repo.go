package repositories

import (
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// RoleRepository reads and writes the roles table — what each role is configured
// with on the "Quyền theo vai trò" screen.
type RoleRepository struct{ db *gorm.DB }

// PermissionOverrides returns the configured tick set of every role that has one
// (roles.permissions not NULL). Roles left out use the built-in defaults.
func (r *RoleRepository) PermissionOverrides() (map[models.Role][]string, error) {
	var rows []models.RoleRecord
	if err := r.db.Where("permissions IS NOT NULL").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[models.Role][]string, len(rows))
	for _, row := range rows {
		out[models.Role(row.Name)] = []string(row.Permissions)
	}
	return out, nil
}

// SetPermissions stores a role's tick set; nil stores NULL (back to the built-in
// defaults). The row is created when missing — an install seeded before the
// role existed has none.
func (r *RoleRepository) SetPermissions(role models.Role, perms models.PermList) error {
	rec := models.RoleRecord{Name: string(role)}
	if err := r.db.Where("name = ?", rec.Name).FirstOrCreate(&rec).Error; err != nil {
		return err
	}
	return r.db.Model(&models.RoleRecord{}).Where("id = ?", rec.ID).Update("permissions", perms).Error
}

// CountFollowing counts, per role, the accounts with no ticks of their own —
// the people a change to that role's ticks reaches.
func (r *RoleRepository) CountFollowing() (map[models.Role]int64, error) {
	var rows []struct {
		Role  models.Role
		Count int64
	}
	err := r.db.Model(&models.User{}).
		Select("role, COUNT(*) AS count").
		Where("permissions IS NULL").
		Group("role").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make(map[models.Role]int64, len(rows))
	for _, row := range rows {
		out[row.Role] = row.Count
	}
	return out, nil
}
