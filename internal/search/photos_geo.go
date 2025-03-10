package search

import (
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/dustin/go-humanize/english"
	"github.com/jinzhu/gorm"

	"github.com/photoprism/photoprism/internal/acl"
	"github.com/photoprism/photoprism/internal/entity"
	"github.com/photoprism/photoprism/internal/event"
	"github.com/photoprism/photoprism/internal/form"
	"github.com/photoprism/photoprism/pkg/fs"
	"github.com/photoprism/photoprism/pkg/pluscode"
	"github.com/photoprism/photoprism/pkg/rnd"
	"github.com/photoprism/photoprism/pkg/s2"
	"github.com/photoprism/photoprism/pkg/txt"
)

// GeoCols specifies the UserPhotosGeo result column names.
var GeoCols = SelectString(GeoResult{}, []string{"*"})

// PhotosGeo finds GeoResults based on the search form without checking rights or permissions.
func PhotosGeo(f form.SearchPhotosGeo) (results GeoResults, err error) {
	return UserPhotosGeo(f, nil)
}

// UserPhotosGeo finds photos based on the search form and user session then returns them as GeoResults.
func UserPhotosGeo(f form.SearchPhotosGeo, sess *entity.Session) (results GeoResults, err error) {
	start := time.Now()

	// Parse query string and filter.
	if err = f.ParseQueryString(); err != nil {
		log.Debugf("search: %s", err)
		return GeoResults{}, ErrBadRequest
	}

	S2Levels := 7

	// Search for nearby photos?
	if f.Near != "" {
		photo := Photo{}

		// Find photo to get location.
		if err = Db().First(&photo, "photo_uid = ?", f.Near).Error; err != nil {
			return GeoResults{}, err
		}

		f.S2 = photo.CellID
		f.Lat = photo.PhotoLat
		f.Lng = photo.PhotoLng

		S2Levels = 12
	}

	// Specify table names and joins.
	s := UnscopedDb().Table(entity.Photo{}.TableName()).Select(GeoCols).
		Joins(`JOIN files ON files.photo_id = photos.id AND files.file_primary = 1 AND files.media_id IS NOT NULL`).
		Joins("LEFT JOIN places ON photos.place_id = places.id").
		Where("photos.deleted_at IS NULL").
		Where("photos.photo_lat <> 0")

	// Accept the album UID as scope for backward compatibility.
	if rnd.IsUID(f.Album, entity.AlbumUID) {
		if txt.Empty(f.Scope) {
			f.Scope = f.Album
		}

		f.Album = ""
	}

	// Limit search results to a specific UID scope, e.g. when sharing.
	if txt.NotEmpty(f.Scope) {
		f.Scope = strings.ToLower(f.Scope)

		if idType, idPrefix := rnd.IdType(f.Scope); idType != rnd.TypeUID || idPrefix != entity.AlbumUID {
			return GeoResults{}, ErrInvalidId
		} else if a, err := entity.CachedAlbumByUID(f.Scope); err != nil || a.AlbumUID == "" {
			return GeoResults{}, ErrInvalidId
		} else if a.AlbumFilter == "" {
			s = s.Joins("JOIN photos_albums ON photos_albums.photo_uid = files.photo_uid").
				Where("photos_albums.hidden = 0 AND photos_albums.album_uid = ?", a.AlbumUID)
		} else if err = form.Unserialize(&f, a.AlbumFilter); err != nil {
			return GeoResults{}, ErrBadFilter
		} else {
			f.Filter = a.AlbumFilter
			s = s.Where("files.photo_uid NOT IN (SELECT photo_uid FROM photos_albums pa WHERE pa.hidden = 1 AND pa.album_uid = ?)", a.AlbumUID)
		}

		S2Levels = 18
	} else {
		f.Scope = ""
	}

	// Check session permissions and apply as needed.
	if sess != nil {
		user := sess.User()
		aclRole := user.AclRole()

		// Exclude private content?
		if acl.Resources.Deny(acl.ResourcePlaces, aclRole, acl.AccessPrivate) {
			f.Public = true
			f.Private = false
		}

		// Exclude archived content?
		if acl.Resources.Deny(acl.ResourcePlaces, aclRole, acl.ActionDelete) {
			f.Archived = false
			f.Review = false
		}

		// Visitors and other restricted users can only access shared content.
		if f.Scope != "" && !sess.HasShare(f.Scope) && (sess.IsVisitor() || sess.NotRegistered()) ||
			f.Scope == "" && acl.Resources.Deny(acl.ResourcePlaces, aclRole, acl.ActionSearch) {
			event.AuditErr([]string{sess.IP(), "session %s", "%s %s as %s", "denied"}, sess.RefID, acl.ActionSearch.String(), string(acl.ResourcePlaces), aclRole)
			return GeoResults{}, ErrForbidden
		}

		// Limit results for external users.
		if f.Scope == "" && acl.Resources.DenyAll(acl.ResourcePlaces, aclRole, acl.Permissions{acl.AccessAll, acl.AccessLibrary}) {
			sharedAlbums := "photos.photo_uid IN (SELECT photo_uid FROM photos_albums WHERE hidden = 0 AND missing = 0 AND album_uid IN (?)) OR "

			if sess.IsVisitor() || sess.NotRegistered() {
				s = s.Where(sharedAlbums+"photos.published_at > ?", sess.SharedUIDs(), entity.TimeStamp())
			} else if basePath := user.GetBasePath(); basePath == "" {
				s = s.Where(sharedAlbums+"photos.created_by = ? OR photos.published_at > ?", sess.SharedUIDs(), user.UserUID, entity.TimeStamp())
			} else {
				s = s.Where(sharedAlbums+"photos.created_by = ? OR photos.published_at > ? OR photos.photo_path = ? OR photos.photo_path LIKE ?",
					sess.SharedUIDs(), user.UserUID, entity.TimeStamp(), basePath, basePath+"/%")
			}
		}
	}

	// Set sort order.
	if f.Near == "" {
		s = s.Order("taken_at, photos.photo_uid")
	} else {
		// Sort by distance to UID.
		s = s.Order(gorm.Expr("(photos.photo_uid = ?) DESC, ABS(? - photos.photo_lat)+ABS(? - photos.photo_lng)", f.Near, f.Lat, f.Lng))
	}

	// Find specific UIDs only?
	if txt.NotEmpty(f.UID) {
		ids := SplitOr(strings.ToLower(f.UID))
		idType, prefix := rnd.ContainsType(ids)

		if idType == rnd.TypeUnknown {
			return GeoResults{}, fmt.Errorf("%s ids specified", idType)
		} else if idType.SHA() {
			s = s.Where("files.file_hash IN (?)", ids)
		} else if idType == rnd.TypeUID {
			switch prefix {
			case entity.PhotoUID:
				s = s.Where("photos.photo_uid IN (?)", ids)
			case entity.FileUID:
				s = s.Where("files.file_uid IN (?)", ids)
			default:
				return GeoResults{}, fmt.Errorf("invalid ids specified")
			}
		}

		// Find UIDs only to improve performance?
		if sess == nil && f.FindUidOnly() {
			// Fetch results.
			if result := s.Scan(&results); result.Error != nil {
				return results, result.Error
			}

			log.Debugf("places: found %s for %s [%s]", english.Plural(len(results), "result", "results"), f.SerializeAll(), time.Since(start))

			return results, nil
		}
	}

	// Find Unique Image ID (Exif), Document ID, or Instance ID (XMP)?
	if txt.NotEmpty(f.ID) {
		for _, id := range SplitAnd(strings.ToLower(f.ID)) {
			if ids := SplitOr(id); len(ids) > 0 {
				s = s.Where("files.instance_id IN (?) OR photos.uuid IN (?)", ids, ids)
			}
		}
	}

	// Set search filters based on search terms.
	if terms := txt.SearchTerms(f.Query); f.Query != "" && len(terms) == 0 {
		if f.Title == "" {
			f.Title = fmt.Sprintf("%s*", strings.Trim(f.Query, "%*"))
			f.Query = ""
		}
	} else if len(terms) > 0 {
		switch {
		case terms["faces"]:
			f.Query = strings.ReplaceAll(f.Query, "faces", "")
			f.Faces = "true"
		case terms["people"]:
			f.Query = strings.ReplaceAll(f.Query, "people", "")
			f.Faces = "true"
		case terms["videos"]:
			f.Query = strings.ReplaceAll(f.Query, "videos", "")
			f.Video = true
		case terms["video"]:
			f.Query = strings.ReplaceAll(f.Query, "video", "")
			f.Video = true
		case terms["vectors"]:
			f.Query = strings.ReplaceAll(f.Query, "vectors", "")
			f.Vector = true
		case terms["vector"]:
			f.Query = strings.ReplaceAll(f.Query, "vector", "")
			f.Vector = true
		case terms["animated"]:
			f.Query = strings.ReplaceAll(f.Query, "animated", "")
			f.Animated = true
		case terms["gifs"]:
			f.Query = strings.ReplaceAll(f.Query, "gifs", "")
			f.Animated = true
		case terms["gif"]:
			f.Query = strings.ReplaceAll(f.Query, "gif", "")
			f.Animated = true
		case terms["live"]:
			f.Query = strings.ReplaceAll(f.Query, "live", "")
			f.Live = true
		case terms["raws"]:
			f.Query = strings.ReplaceAll(f.Query, "raws", "")
			f.Raw = true
		case terms["raw"]:
			f.Query = strings.ReplaceAll(f.Query, "raw", "")
			f.Raw = true
		case terms["favorites"]:
			f.Query = strings.ReplaceAll(f.Query, "favorites", "")
			f.Favorite = true
		case terms["panoramas"]:
			f.Query = strings.ReplaceAll(f.Query, "panoramas", "")
			f.Panorama = true
		case terms["scans"]:
			f.Query = strings.ReplaceAll(f.Query, "scans", "")
			f.Scan = true
		}
	}

	// Filter by label, label category, and keywords?
	if f.Query != "" {
		var categories []entity.Category
		var labels []entity.Label
		var labelIds []uint

		if err := Db().Where(AnySlug("custom_slug", f.Query, " ")).Find(&labels).Error; len(labels) == 0 || err != nil {
			log.Debugf("search: label %s not found, using fuzzy search", txt.LogParamLower(f.Query))

			for _, where := range LikeAnyKeyword("k.keyword", f.Query) {
				s = s.Where("photos.id IN (SELECT pk.photo_id FROM keywords k JOIN photos_keywords pk ON k.id = pk.keyword_id WHERE (?))", gorm.Expr(where))
			}
		} else {
			for _, l := range labels {
				labelIds = append(labelIds, l.ID)

				Log("find categories", Db().Where("category_id = ?", l.ID).Find(&categories).Error)
				log.Debugf("search: label %s includes %d categories", txt.LogParamLower(l.LabelName), len(categories))

				for _, category := range categories {
					labelIds = append(labelIds, category.LabelID)
				}
			}

			if wheres := LikeAnyKeyword("k.keyword", f.Query); len(wheres) > 0 {
				for _, where := range wheres {
					s = s.Where("photos.id IN (SELECT pk.photo_id FROM keywords k JOIN photos_keywords pk ON k.id = pk.keyword_id WHERE (?)) OR "+
						"photos.id IN (SELECT pl.photo_id FROM photos_labels pl WHERE pl.uncertainty < 100 AND pl.label_id IN (?))", gorm.Expr(where), labelIds)
				}
			} else {
				s = s.Where("photos.id IN (SELECT pl.photo_id FROM photos_labels pl WHERE pl.uncertainty < 100 AND pl.label_id IN (?))", labelIds)
			}
		}
	}

	// Search for one or more keywords?
	if f.Keywords != "" {
		for _, where := range LikeAnyWord("k.keyword", f.Keywords) {
			s = s.Where("photos.id IN (SELECT pk.photo_id FROM keywords k JOIN photos_keywords pk ON k.id = pk.keyword_id WHERE (?))", gorm.Expr(where))
		}
	}

	// Filter by number of faces?
	if txt.IsUInt(f.Faces) {
		s = s.Where("photos.photo_faces >= ?", txt.Int(f.Faces))
	} else if txt.New(f.Faces) && f.Face == "" {
		f.Face = f.Faces
		f.Faces = ""
	} else if txt.Yes(f.Faces) {
		s = s.Where("photos.photo_faces > 0")
	} else if txt.No(f.Faces) {
		s = s.Where("photos.photo_faces = 0")
	}

	// Filter for specific face clusters? Example: PLJ7A3G4MBGZJRMVDIUCBLC46IAP4N7O
	if len(f.Face) >= 32 {
		for _, f := range SplitAnd(strings.ToUpper(f.Face)) {
			s = s.Where(fmt.Sprintf("photos.id IN (SELECT photo_id FROM files f JOIN %s m ON f.file_uid = m.file_uid AND m.marker_invalid = 0 WHERE face_id IN (?))",
				entity.Marker{}.TableName()), SplitOr(f))
		}
	} else if txt.New(f.Face) {
		s = s.Where(fmt.Sprintf("photos.id IN (SELECT photo_id FROM files f JOIN %s m ON f.file_uid = m.file_uid AND m.marker_invalid = 0 AND m.marker_type = ? WHERE subj_uid IS NULL OR subj_uid = '')",
			entity.Marker{}.TableName()), entity.MarkerFace)
	} else if txt.No(f.Face) {
		s = s.Where(fmt.Sprintf("photos.id IN (SELECT photo_id FROM files f JOIN %s m ON f.file_uid = m.file_uid AND m.marker_invalid = 0 AND m.marker_type = ? WHERE face_id IS NULL OR face_id = '')",
			entity.Marker{}.TableName()), entity.MarkerFace)
	} else if txt.Yes(f.Face) {
		s = s.Where(fmt.Sprintf("photos.id IN (SELECT photo_id FROM files f JOIN %s m ON f.file_uid = m.file_uid AND m.marker_invalid = 0 AND m.marker_type = ? WHERE face_id IS NOT NULL AND face_id <> '')",
			entity.Marker{}.TableName()), entity.MarkerFace)
	}

	// Filter for one or more subjects?
	if f.Subject != "" {
		for _, subj := range SplitAnd(strings.ToLower(f.Subject)) {
			if subjects := SplitOr(subj); rnd.ContainsUID(subjects, 'j') {
				s = s.Where(fmt.Sprintf("photos.id IN (SELECT photo_id FROM files f JOIN %s m ON f.file_uid = m.file_uid AND m.marker_invalid = 0 WHERE subj_uid IN (?))",
					entity.Marker{}.TableName()), subjects)
			} else {
				s = s.Where(fmt.Sprintf("photos.id IN (SELECT photo_id FROM files f JOIN %s m ON f.file_uid = m.file_uid AND m.marker_invalid = 0 JOIN %s s ON s.subj_uid = m.subj_uid WHERE (?))",
					entity.Marker{}.TableName(), entity.Subject{}.TableName()), gorm.Expr(AnySlug("s.subj_slug", subj, txt.Or)))
			}
		}
	} else if f.Subjects != "" {
		for _, where := range LikeAllNames(Cols{"subj_name", "subj_alias"}, f.Subjects) {
			s = s.Where(fmt.Sprintf("photos.id IN (SELECT photo_id FROM files f JOIN %s m ON f.file_uid = m.file_uid AND m.marker_invalid = 0 JOIN %s s ON s.subj_uid = m.subj_uid WHERE (?))",
				entity.Marker{}.TableName(), entity.Subject{}.TableName()), gorm.Expr(where))
		}
	}

	// Find photos in albums or not in an album, unless search results are limited to a scope.
	if f.Scope == "" {
		if f.Unsorted {
			s = s.Where("photos.photo_uid NOT IN (SELECT photo_uid FROM photos_albums pa JOIN albums a ON a.album_uid = pa.album_uid WHERE pa.hidden = 0 AND a.deleted_at IS NULL)")
		} else if txt.NotEmpty(f.Album) {
			v := strings.Trim(f.Album, "*%") + "%"
			s = s.Where("photos.photo_uid IN (SELECT pa.photo_uid FROM photos_albums pa JOIN albums a ON a.album_uid = pa.album_uid AND pa.hidden = 0 WHERE (a.album_title LIKE ? OR a.album_slug LIKE ?))", v, v)
		} else if txt.NotEmpty(f.Albums) {
			for _, where := range LikeAnyWord("a.album_title", f.Albums) {
				s = s.Where("photos.photo_uid IN (SELECT pa.photo_uid FROM photos_albums pa JOIN albums a ON a.album_uid = pa.album_uid AND pa.hidden = 0 WHERE (?))", gorm.Expr(where))
			}
		}
	}

	// Filter by camera?
	if f.Camera > 0 {
		s = s.Where("photos.camera_id = ?", f.Camera)
	}

	// Filter by camera lens?
	if f.Lens > 0 {
		s = s.Where("photos.lens_id = ?", f.Lens)
	}

	// Filter by year?
	if f.Year != "" {
		s = s.Where(AnyInt("photos.photo_year", f.Year, txt.Or, entity.UnknownYear, txt.YearMax))
	}

	// Filter by month?
	if f.Month != "" {
		s = s.Where(AnyInt("photos.photo_month", f.Month, txt.Or, entity.UnknownMonth, txt.MonthMax))
	}

	// Filter by day?
	if f.Day != "" {
		s = s.Where(AnyInt("photos.photo_day", f.Day, txt.Or, entity.UnknownDay, txt.DayMax))
	}

	// Filter by main color?
	if f.Color != "" {
		s = s.Where("files.file_main_color IN (?)", SplitOr(strings.ToLower(f.Color)))
	}

	// Find favorites only?
	if f.Favorite {
		s = s.Where("photos.photo_favorite = 1")
	}

	// Find scans only?
	if f.Scan {
		s = s.Where("photos.photo_scan = 1")
	}

	// Find panoramas only?
	if f.Panorama {
		s = s.Where("photos.photo_panorama = 1")
	}

	// Find portrait/landscape/square pictures only?
	if f.Portrait {
		s = s.Where("files.file_portrait = 1")
	} else if f.Landscape {
		s = s.Where("files.file_aspect_ratio > 1.25")
	} else if f.Square {
		s = s.Where("files.file_aspect_ratio = 1")
	}

	// Filter by location country?
	if f.Country != "" {
		s = s.Where("photos.photo_country IN (?)", SplitOr(strings.ToLower(f.Country)))
	}

	// Filter by location state?
	if txt.NotEmpty(f.State) {
		s = s.Where("places.place_state IN (?)", SplitOr(f.State))
	}

	// Filter by location city?
	if txt.NotEmpty(f.City) {
		s = s.Where("places.place_city IN (?)", SplitOr(f.City))
	}

	// Filter by media type?
	if txt.NotEmpty(f.Type) {
		s = s.Where("photos.photo_type IN (?)", SplitOr(strings.ToLower(f.Type)))
	} else if f.Video {
		s = s.Where("photos.photo_type = ?", entity.MediaVideo)
	} else if f.Vector {
		s = s.Where("photos.photo_type = ?", entity.MediaVector)
	} else if f.Animated {
		s = s.Where("photos.photo_type = ?", entity.MediaAnimated)
	} else if f.Raw {
		s = s.Where("photos.photo_type = ?", entity.MediaRaw)
	} else if f.Live {
		s = s.Where("photos.photo_type = ?", entity.MediaLive)
	} else if f.Photo {
		s = s.Where("photos.photo_type IN ('image','raw','live','animated')")
	}

	// Filter by storage path?
	if f.Path != "" {
		p := f.Path

		if strings.HasPrefix(p, "/") {
			p = p[1:]
		}

		if strings.HasSuffix(p, "/") {
			s = s.Where("photos.photo_path = ?", p[:len(p)-1])
		} else {
			where, values := OrLike("photos.photo_path", p)
			s = s.Where(where, values...)
		}
	}

	// Filter by primary file name without path and extension?
	if f.Name != "" {
		where, names := OrLike("photos.photo_name", f.Name)

		// Omit file path and known extensions.
		for i := range names {
			names[i] = fs.StripKnownExt(path.Base(names[i].(string)))
		}

		s = s.Where(where, names...)
	}

	// Filter by photo title?
	if f.Title != "" {
		where, values := OrLike("photos.photo_title", f.Title)
		s = s.Where(where, values...)
	}

	// Filter by status?
	if f.Archived {
		s = s.Where("photos.photo_quality > -1")
		s = s.Where("photos.deleted_at IS NOT NULL")
	} else {
		s = s.Where("photos.deleted_at IS NULL")

		if f.Private {
			s = s.Where("photos.photo_private = 1")
		} else if f.Public {
			s = s.Where("photos.photo_private = 0")
		}

		if f.Review {
			s = s.Where("photos.photo_quality < 3")
		} else if f.Quality != 0 && f.Private == false {
			s = s.Where("photos.photo_quality >= ?", f.Quality)
		}
	}

	if f.S2 != "" {
		s2Min, s2Max := s2.PrefixedRange(f.S2, S2Levels)
		s = s.Where("photos.cell_id BETWEEN ? AND ?", s2Min, s2Max)
	} else if f.Olc != "" {
		s2Min, s2Max := s2.PrefixedRange(pluscode.S2(f.Olc), S2Levels)
		s = s.Where("photos.cell_id BETWEEN ? AND ?", s2Min, s2Max)
	} else {
		// Filter by approx distance to coordinate:
		if f.Lat != 0 {
			latMin := f.Lat - Radius*float32(f.Dist)
			latMax := f.Lat + Radius*float32(f.Dist)
			s = s.Where("photos.photo_lat BETWEEN ? AND ?", latMin, latMax)
		}
		if f.Lng != 0 {
			lngMin := f.Lng - Radius*float32(f.Dist)
			lngMax := f.Lng + Radius*float32(f.Dist)
			s = s.Where("photos.photo_lng BETWEEN ? AND ?", lngMin, lngMax)
		}
	}

	// Find photos taken before date?
	if !f.Before.IsZero() {
		s = s.Where("photos.taken_at <= ?", f.Before.Format("2006-01-02"))
	}

	// Find photos taken after date?
	if !f.After.IsZero() {
		s = s.Where("photos.taken_at >= ?", f.After.Format("2006-01-02"))
	}

	// Limit offset and count.
	if f.Count > 0 {
		s = s.Limit(f.Count).Offset(f.Offset)
	} else {
		s = s.Limit(1000000).Offset(f.Offset)
	}

	// Fetch results.
	if result := s.Scan(&results); result.Error != nil {
		return results, result.Error
	}

	log.Debugf("places: found %s for %s [%s]", english.Plural(len(results), "result", "results"), f.SerializeAll(), time.Since(start))

	return results, nil
}
