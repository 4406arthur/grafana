package sqlstore

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/grafana/grafana/pkg/bus"
	"github.com/grafana/grafana/pkg/metrics"
	m "github.com/grafana/grafana/pkg/models"
	"github.com/grafana/grafana/pkg/services/search"
)

func init() {
	bus.AddHandler("sql", SaveDashboard)
	bus.AddHandler("sql", GetDashboard)
	bus.AddHandler("sql", GetDashboards)
	bus.AddHandler("sql", DeleteDashboard)
	bus.AddHandler("sql", SearchDashboards)
	bus.AddHandler("sql", GetDashboardTags)
	bus.AddHandler("sql", GetDashboardSlugById)
	bus.AddHandler("sql", GetDashboardsByPluginId)
}

func SaveDashboard(cmd *m.SaveDashboardCommand) error {
	return inTransaction(func(sess *DBSession) error {
		dash := cmd.GetDashboardModel()

		// try get existing dashboard
		var existing, sameTitle m.Dashboard

		if dash.Id > 0 {
			dashWithIdExists, err := sess.Where("id=? AND org_id=?", dash.Id, dash.OrgId).Get(&existing)
			if err != nil {
				return err
			}
			if !dashWithIdExists {
				return m.ErrDashboardNotFound
			}

			// check for is someone else has written in between
			if dash.Version != existing.Version {
				if cmd.Overwrite {
					dash.Version = existing.Version
				} else {
					return m.ErrDashboardVersionMismatch
				}
			}

			// do not allow plugin dashboard updates without overwrite flag
			if existing.PluginId != "" && cmd.Overwrite == false {
				return m.UpdatePluginDashboardError{PluginId: existing.PluginId}
			}
		}

		sameTitleExists, err := sess.Where("org_id=? AND slug=?", dash.OrgId, dash.Slug).Get(&sameTitle)
		if err != nil {
			return err
		}

		if sameTitleExists {
			// another dashboard with same name
			if dash.Id != sameTitle.Id {
				if cmd.Overwrite {
					dash.Id = sameTitle.Id
					dash.Version = sameTitle.Version
				} else {
					return m.ErrDashboardWithSameNameExists
				}
			}
		}

		err = setHasAcl(sess, dash)
		if err != nil {
			return err
		}

		parentVersion := dash.Version
		affectedRows := int64(0)

		if dash.Id == 0 {
			dash.Version = 1
			metrics.M_Models_Dashboard_Insert.Inc(1)
			dash.Data.Set("version", dash.Version)
			affectedRows, err = sess.Insert(dash)
		} else {
			dash.Version++
			dash.Data.Set("version", dash.Version)
			affectedRows, err = sess.MustCols("folder_id", "has_acl").Id(dash.Id).Update(dash)
		}

		if err != nil {
			return err
		}

		if affectedRows == 0 {
			return m.ErrDashboardNotFound
		}

		dashVersion := &m.DashboardVersion{
			DashboardId:   dash.Id,
			ParentVersion: parentVersion,
			RestoredFrom:  cmd.RestoredFrom,
			Version:       dash.Version,
			Created:       time.Now(),
			CreatedBy:     dash.UpdatedBy,
			Message:       cmd.Message,
			Data:          dash.Data,
		}

		// insert version entry
		if affectedRows, err = sess.Insert(dashVersion); err != nil {
			return err
		} else if affectedRows == 0 {
			return m.ErrDashboardNotFound
		}

		// delete existing tags
		_, err = sess.Exec("DELETE FROM dashboard_tag WHERE dashboard_id=?", dash.Id)
		if err != nil {
			return err
		}

		// insert new tags
		tags := dash.GetTags()
		if len(tags) > 0 {
			for _, tag := range tags {
				if _, err := sess.Insert(&DashboardTag{DashboardId: dash.Id, Term: tag}); err != nil {
					return err
				}
			}
		}
		cmd.Result = dash

		return err
	})
}

func setHasAcl(sess *DBSession, dash *m.Dashboard) error {
	// check if parent has acl
	if dash.FolderId > 0 {
		var parent m.Dashboard
		if hasParent, err := sess.Where("folder_id=?", dash.FolderId).Get(&parent); err != nil {
			return err
		} else if hasParent && parent.HasAcl {
			dash.HasAcl = true
		}
	}

	// check if dash has its own acl
	if dash.Id > 0 {
		if res, err := sess.Query("SELECT 1 from dashboard_acl WHERE dashboard_id =?", dash.Id); err != nil {
			return err
		} else {
			if len(res) > 0 {
				dash.HasAcl = true
			}
		}
	}

	return nil
}

func GetDashboard(query *m.GetDashboardQuery) error {
	dashboard := m.Dashboard{Slug: query.Slug, OrgId: query.OrgId, Id: query.Id}
	has, err := x.Get(&dashboard)

	if err != nil {
		return err
	} else if has == false {
		return m.ErrDashboardNotFound
	}

	dashboard.Data.Set("id", dashboard.Id)
	query.Result = &dashboard
	return nil
}

type DashboardSearchProjection struct {
	Id          int64
	Title       string
	Slug        string
	Term        string
	IsFolder    bool
	FolderId    int64
	FolderSlug  string
	FolderTitle string
}

func findDashboards(query *search.FindPersistedDashboardsQuery) ([]DashboardSearchProjection, error) {
	limit := query.Limit
	if limit == 0 {
		limit = 1000
	}

	var sql bytes.Buffer
	params := make([]interface{}, 0)

	sql.WriteString(`
	SELECT
		dashboard.id,
		dashboard.title,
		dashboard.slug,
		dashboard_tag.term,
		dashboard.is_folder,
		dashboard.folder_id,
		folder.slug as folder_slug,
		folder.title as folder_title
	FROM (
		SELECT
			dashboard.id FROM dashboard
			LEFT OUTER JOIN dashboard_tag ON dashboard_tag.dashboard_id = dashboard.id
	`)

	// add tags filter
	if len(query.Tags) > 0 {
		sql.WriteString(` WHERE dashboard_tag.term IN (?` + strings.Repeat(",?", len(query.Tags)-1) + `)`)
		for _, tag := range query.Tags {
			params = append(params, tag)
		}
	}

	// this ends the inner select (tag filtered part)
	sql.WriteString(`
		  GROUP BY dashboard.id HAVING COUNT(dashboard.id) >= ?
		  ORDER BY dashboard.title ASC LIMIT ?) as ids`)
	params = append(params, len(query.Tags))
	params = append(params, limit)

	sql.WriteString(`
		INNER JOIN dashboard on ids.id = dashboard.id
		LEFT OUTER JOIN dashboard folder on folder.id = dashboard.folder_id
		LEFT OUTER JOIN dashboard_tag on dashboard.id = dashboard_tag.dashboard_id`)
	if query.IsStarred {
		sql.WriteString(" INNER JOIN star on star.dashboard_id = dashboard.id")
	}

	sql.WriteString(` WHERE dashboard.org_id=?`)
	params = append(params, query.SignedInUser.OrgId)

	if query.IsStarred {
		sql.WriteString(` AND star.user_id=?`)
		params = append(params, query.SignedInUser.UserId)
	}

	if len(query.DashboardIds) > 0 {
		sql.WriteString(` AND dashboard.id IN (?` + strings.Repeat(",?", len(query.DashboardIds)-1) + `)`)
		for _, dashboardId := range query.DashboardIds {
			params = append(params, dashboardId)
		}
	}

	if query.SignedInUser.OrgRole != m.ROLE_ADMIN {
		allowedDashboardsSubQuery := ` AND (dashboard.has_acl = 0 OR dashboard.id in (
		SELECT distinct d.id AS DashboardId
			FROM dashboard AS d
	      LEFT JOIN dashboard_acl as da on d.folder_id = da.dashboard_id or d.id = da.dashboard_id
	      LEFT JOIN user_group_member as ugm on ugm.user_group_id =  da.user_group_id
	      LEFT JOIN org_user ou on ou.role = da.role
			WHERE
			  d.has_acl = 1 and
				(da.user_id = ? or ugm.user_id = ? or ou.id is not null)
			  and d.org_id = ?
			  ))`

		sql.WriteString(allowedDashboardsSubQuery)
		params = append(params, query.SignedInUser.UserId, query.SignedInUser.UserId, query.SignedInUser.OrgId)
	}

	if len(query.Title) > 0 {
		sql.WriteString(" AND dashboard.title " + dialect.LikeStr() + " ?")
		params = append(params, "%"+query.Title+"%")
	}

	if len(query.Type) > 0 && query.Type == "dash-folder" {
		sql.WriteString(" AND dashboard.is_folder = 1")
	}

	if len(query.Type) > 0 && query.Type == "dash-db" {
		sql.WriteString(" AND dashboard.is_folder = 0")
	}

	if query.FolderId > 0 {
		sql.WriteString(" AND dashboard.folder_id = ?")
		params = append(params, query.FolderId)
	}

	sql.WriteString(fmt.Sprintf(" ORDER BY dashboard.title ASC LIMIT 1000"))

	var res []DashboardSearchProjection

	err := x.Sql(sql.String(), params...).Find(&res)
	if err != nil {
		return nil, err
	}

	return res, nil
}

func SearchDashboards(query *search.FindPersistedDashboardsQuery) error {
	res, err := findDashboards(query)
	if err != nil {
		return err
	}

	makeQueryResult(query, res)

	return nil
}

func getHitType(item DashboardSearchProjection) search.HitType {
	var hitType search.HitType
	if item.IsFolder {
		hitType = search.DashHitFolder
	} else {
		hitType = search.DashHitDB
	}

	return hitType
}

func makeQueryResult(query *search.FindPersistedDashboardsQuery, res []DashboardSearchProjection) {
	query.Result = make([]*search.Hit, 0)
	hits := make(map[int64]*search.Hit)

	for _, item := range res {
		hit, exists := hits[item.Id]
		if !exists {
			hit = &search.Hit{
				Id:          item.Id,
				Title:       item.Title,
				Uri:         "db/" + item.Slug,
				Type:        getHitType(item),
				FolderId:    item.FolderId,
				FolderTitle: item.FolderTitle,
				FolderSlug:  item.FolderSlug,
				Tags:        []string{},
			}
			query.Result = append(query.Result, hit)
			hits[item.Id] = hit
		}
		if len(item.Term) > 0 {
			hit.Tags = append(hit.Tags, item.Term)
		}
	}
}

func GetDashboardTags(query *m.GetDashboardTagsQuery) error {
	sql := `SELECT
					  COUNT(*) as count,
						term
					FROM dashboard
					INNER JOIN dashboard_tag on dashboard_tag.dashboard_id = dashboard.id
					WHERE dashboard.org_id=?
					GROUP BY term`

	query.Result = make([]*m.DashboardTagCloudItem, 0)
	sess := x.Sql(sql, query.OrgId)
	err := sess.Find(&query.Result)
	return err
}

func DeleteDashboard(cmd *m.DeleteDashboardCommand) error {
	return inTransaction(func(sess *DBSession) error {
		dashboard := m.Dashboard{Id: cmd.Id, OrgId: cmd.OrgId}
		has, err := sess.Get(&dashboard)
		if err != nil {
			return err
		} else if has == false {
			return m.ErrDashboardNotFound
		}

		deletes := []string{
			"DELETE FROM dashboard_tag WHERE dashboard_id = ? ",
			"DELETE FROM star WHERE dashboard_id = ? ",
			"DELETE FROM dashboard WHERE id = ?",
			"DELETE FROM playlist_item WHERE type = 'dashboard_by_id' AND value = ?",
			"DELETE FROM dashboard_version WHERE dashboard_id = ?",
			"DELETE FROM dashboard WHERE folder_id = ?",
		}

		for _, sql := range deletes {
			_, err := sess.Exec(sql, dashboard.Id)
			if err != nil {
				return err
			}
		}

		if err := DeleteAlertDefinition(dashboard.Id, sess); err != nil {
			return nil
		}

		return nil
	})
}

func GetDashboards(query *m.GetDashboardsQuery) error {
	if len(query.DashboardIds) == 0 {
		return m.ErrCommandValidationFailed
	}

	var dashboards = make([]*m.Dashboard, 0)

	err := x.In("id", query.DashboardIds).Find(&dashboards)
	query.Result = dashboards

	if err != nil {
		return err
	}

	return nil
}

func GetDashboardsByPluginId(query *m.GetDashboardsByPluginIdQuery) error {
	var dashboards = make([]*m.Dashboard, 0)

	err := x.Where("org_id=? AND plugin_id=?", query.OrgId, query.PluginId).Find(&dashboards)
	query.Result = dashboards

	if err != nil {
		return err
	}

	return nil
}

type DashboardSlugDTO struct {
	Slug string
}

func GetDashboardSlugById(query *m.GetDashboardSlugByIdQuery) error {
	var rawSql = `SELECT slug from dashboard WHERE Id=?`
	var slug = DashboardSlugDTO{}

	exists, err := x.Sql(rawSql, query.Id).Get(&slug)

	if err != nil {
		return err
	} else if exists == false {
		return m.ErrDashboardNotFound
	}

	query.Result = slug.Slug
	return nil
}
