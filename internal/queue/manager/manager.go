package manager

import (
	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"gorm.io/gorm"
)

type manager struct {
	dao *dao.Query
	db  *gorm.DB
}
