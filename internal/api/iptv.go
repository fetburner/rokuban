package api

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	iptvPlaylistPath = "/api/iptv/playlist.m3u"
	xmltvGuidePath   = "/api/iptv/xmltv.xml"
	defaultXMLTVSpan = 7 * 24 * time.Hour
	defaultXMLTVPast = 3 * time.Hour
)

type iptvScope struct {
	live       bool
	recordings bool
}

type iptvService struct {
	site        string
	networkID   int32
	serviceID   int32
	name        string
	channel     string
	channelType string
}

// ExportIPTV は既存のライブ配信・録画配信 URL を含む M3U を返す。
// 出力は JSON API ではないため OpenAPI の生成ハンドラを通さない。
func (h *Server) ExportIPTV(w http.ResponseWriter, r *http.Request) {
	scope, err := parseIPTVScope(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	query := r.URL.Query()
	liveProfile, err := singleQueryValue(query, "liveProfile")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	recordingProfile, err := singleQueryValue(query, "recordingProfile")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if liveProfile != "" && !scope.live {
		http.Error(w, "liveProfile requires live entries", http.StatusBadRequest)
		return
	}
	if recordingProfile != "" && !scope.recordings {
		http.Error(w, "recordingProfile requires recording entries", http.StatusBadRequest)
		return
	}
	if liveProfile != "" && !h.hasLiveProfile(liveProfile) {
		http.Error(w, "unknown live profile", http.StatusBadRequest)
		return
	}
	if recordingProfile != "" && h.encodeProfiles != nil {
		if _, ok := h.encodeProfiles[recordingProfile]; !ok {
			http.Error(w, "unknown recording profile", http.StatusBadRequest)
			return
		}
	}
	if scope.live && !h.capabilities.Live && !scope.recordings {
		http.NotFound(w, r)
		return
	}
	if h.pool == nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}

	var out bytes.Buffer
	out.WriteString("#EXTM3U url-tvg=\"" + xmltvGuidePath + "\"\n")
	if scope.live && h.capabilities.Live {
		if err := h.writeIPTVServices(r.Context(), &out, liveProfile); err != nil {
			slog.Error("api: exporting IPTV services", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	if scope.recordings {
		if err := h.writeIPTVRecordings(r.Context(), &out, recordingProfile); err != nil {
			slog.Error("api: exporting IPTV recordings", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(out.Bytes())
}

func parseIPTVScope(query url.Values) (iptvScope, error) {
	value, err := singleQueryValue(query, "include")
	if err != nil {
		return iptvScope{}, err
	}
	switch value {
	case "", "all":
		return iptvScope{live: true, recordings: true}, nil
	case "live":
		return iptvScope{live: true}, nil
	case "recordings":
		return iptvScope{recordings: true}, nil
	default:
		return iptvScope{}, fmt.Errorf("include must be all, live or recordings")
	}
}

func singleQueryValue(query url.Values, key string) (string, error) {
	values := query[key]
	if len(values) > 1 {
		return "", fmt.Errorf("%s may be specified only once", key)
	}
	if len(values) == 0 {
		return "", nil
	}
	return strings.TrimSpace(values[0]), nil
}

func (h *Server) hasLiveProfile(name string) bool {
	for _, profile := range h.liveProfiles {
		if profile.Name == name {
			return true
		}
	}
	return false
}

func (h *Server) writeIPTVServices(ctx context.Context, out *bytes.Buffer, profile string) error {
	rows, err := h.pool.Query(ctx, `
SELECT site, network_id, service_id, name, channel, channel_type
FROM epg_services
WHERE site = ANY($1::text[])
ORDER BY site, channel_type, remote_control_key_id, network_id, service_id`, h.siteNames)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var service iptvService
		if err := rows.Scan(&service.site, &service.networkID, &service.serviceID, &service.name, &service.channel, &service.channelType); err != nil {
			return err
		}
		id := exportChannelID(service.site, service.networkID, service.serviceID)
		name := nonEmpty(service.name, "番組名なし")
		group := "ライブ"
		if service.channelType != "" {
			group += " / " + service.channelType
		}
		if len(h.siteNames) > 1 {
			group += " / " + service.site
		}
		fmt.Fprintf(out, "#EXTINF:-1 tvg-id=\"%s\" tvg-name=\"%s\" group-title=\"%s\",%s\n",
			id, m3uAttribute(name), m3uAttribute(group), m3uText(name))
		fmt.Fprintln(out, livePlaylistURL(service, profile))
	}
	return rows.Err()
}

func (h *Server) writeIPTVRecordings(ctx context.Context, out *bytes.Buffer, profile string) error {
	rows, err := h.pool.Query(ctx, `
SELECT r.id, r.site, r.network_id, r.service_id, r.title
FROM recordings r
JOIN media_assets a ON a.recording_id = r.id
WHERE r.site = ANY($1::text[])
  AND r.status = 'finished'
  AND r.deleted_at IS NULL
  AND r.superseded_at IS NULL
  AND a.state = 'active'
  AND (
    ($2::text IS NULL AND a.kind = 'original' AND a.profile IS NULL)
    OR ($2::text IS NOT NULL AND a.kind = 'encoded' AND a.profile = $2::text)
  )
ORDER BY r.program_start_at DESC, r.id DESC`, h.siteNames, nullableText(profile))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var site string
		var networkID, serviceID int32
		var title string
		if err := rows.Scan(&id, &site, &networkID, &serviceID, &title); err != nil {
			return err
		}
		name := nonEmpty(title, "番組名なし")
		channelID := exportChannelID(site, networkID, serviceID)
		fmt.Fprintf(out, "#EXTINF:-1 tvg-id=\"%s\" tvg-name=\"%s\" group-title=\"録画 / %s\",%s\n",
			channelID, m3uAttribute(name), m3uAttribute(site), m3uText(name))
		fmt.Fprintln(out, recordingFileURL(id, profile))
	}
	return rows.Err()
}

func nullableText(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func exportChannelID(site string, networkID, serviceID int32) string {
	return fmt.Sprintf("rokuban.%s.%d.%d", site, networkID, serviceID)
}

func livePlaylistURL(service iptvService, profile string) string {
	path := fmt.Sprintf("/api/sites/%s/networks/%d/services/%d/live/playlist.m3u8",
		url.PathEscape(service.site), service.networkID, service.serviceID)
	if profile == "" {
		return path
	}
	return path + "?profile=" + url.QueryEscape(profile)
}

func recordingFileURL(id int64, profile string) string {
	path := fmt.Sprintf("/api/media/recordings/%d/file", id)
	if profile == "" {
		return path
	}
	return path + "?profile=" + url.QueryEscape(profile)
}

func m3uText(value string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return ' '
		}
		if r == 0 {
			return -1
		}
		return r
	}, value)
}

func m3uAttribute(value string) string {
	value = m3uText(value)
	value = strings.ReplaceAll(value, "&", "&amp;")
	return strings.ReplaceAll(value, `"`, "&quot;")
}

func nonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

type xmlTVDocument struct {
	XMLName       xml.Name         `xml:"tv"`
	Date          string           `xml:"date,attr"`
	GeneratorInfo string           `xml:"generator-info-name,attr"`
	Channels      []xmlTVChannel   `xml:"channel"`
	Programmes    []xmlTVProgramme `xml:"programme"`
}

type xmlTVChannel struct {
	ID          string      `xml:"id,attr"`
	DisplayName []xmlTVText `xml:"display-name"`
	URL         *xmlTVURL   `xml:"url,omitempty"`
}

type xmlTVProgramme struct {
	Start   string     `xml:"start,attr"`
	Stop    string     `xml:"stop,attr,omitempty"`
	Channel string     `xml:"channel,attr"`
	Title   xmlTVText  `xml:"title"`
	Desc    *xmlTVText `xml:"desc,omitempty"`
	URL     xmlTVURL   `xml:"url"`
}

type xmlTVText struct {
	Lang string `xml:"lang,attr,omitempty"`
	Text string `xml:",chardata"`
}

type xmlTVURL struct {
	System string `xml:"system,attr,omitempty"`
	Value  string `xml:",chardata"`
}

// ExportXMLTV は EPG 射影を XMLTV として出力する。
// 番組・サービスの正本や API の資源同定は変更しない。
func (h *Server) ExportXMLTV(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	now := time.Now().UTC()
	windowStart := now.Add(-defaultXMLTVPast)
	windowEnd := windowStart.Add(defaultXMLTVSpan)
	rows, err := h.pool.Query(r.Context(), `
SELECT s.site, s.network_id, s.service_id, s.name, s.channel_type, s.channel,
       p.program_id, p.start_at, p.end_at, p.name, p.description
FROM epg_services s
LEFT JOIN epg_programs p
  ON p.site = s.site
 AND p.network_id = s.network_id
 AND p.service_id = s.service_id
 AND p.start_at < $3::timestamptz
 AND (p.end_at > $2::timestamptz OR p.start_at >= $2::timestamptz)
WHERE s.site = ANY($1::text[])
ORDER BY s.site, s.network_id, s.service_id, p.start_at, p.program_id`,
		h.siteNames, windowStart, windowEnd)
	if err != nil {
		slog.Error("api: exporting XMLTV guide", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	doc := xmlTVDocument{
		Date:          formatXMLTVTime(now),
		GeneratorInfo: "Rokuban",
		Channels:      make([]xmlTVChannel, 0),
		Programmes:    make([]xmlTVProgramme, 0),
	}
	channelIDs := make(map[string]struct{})
	for rows.Next() {
		var site, serviceName, channelType, channel string
		var networkID, serviceID int32
		var programID *int64
		var startAt, endAt *time.Time
		var title, description *string
		if err := rows.Scan(&site, &networkID, &serviceID, &serviceName, &channelType, &channel,
			&programID, &startAt, &endAt, &title, &description); err != nil {
			slog.Error("api: scanning XMLTV projection", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		channelID := exportChannelID(site, networkID, serviceID)
		if _, exists := channelIDs[channelID]; !exists {
			channelIDs[channelID] = struct{}{}
			name := nonEmpty(serviceName, "チャンネル名なし")
			displayNames := []xmlTVText{{Lang: "ja", Text: name}}
			if channel != "" {
				displayNames = append(displayNames, xmlTVText{Lang: "ja", Text: strings.TrimSpace(channelType + " " + channel)})
			}
			if len(h.siteNames) > 1 {
				displayNames = append(displayNames, xmlTVText{Lang: "ja", Text: site + " / " + name})
			}
			xmlChannel := xmlTVChannel{
				ID:          channelID,
				DisplayName: displayNames,
			}
			if h.capabilities.Live {
				xmlChannel.URL = &xmlTVURL{
					System: "rokuban-live",
					Value:  livePlaylistURL(iptvService{site: site, networkID: networkID, serviceID: serviceID}, ""),
				}
			}
			doc.Channels = append(doc.Channels, xmlChannel)
		}
		if programID == nil || startAt == nil || title == nil {
			continue
		}
		programme := xmlTVProgramme{
			Start:   formatXMLTVTime(*startAt),
			Channel: channelID,
			Title:   xmlTVText{Lang: "ja", Text: nonEmpty(*title, "番組名なし")},
			URL: xmlTVURL{
				System: "rokuban-api",
				Value:  programAPIURL(site, *programID),
			},
		}
		if endAt != nil && endAt.After(*startAt) {
			programme.Stop = formatXMLTVTime(*endAt)
		}
		if description != nil && strings.TrimSpace(*description) != "" {
			programme.Desc = &xmlTVText{Lang: "ja", Text: *description}
		}
		doc.Programmes = append(doc.Programmes, programme)
	}
	if err := rows.Err(); err != nil {
		slog.Error("api: iterating XMLTV projection", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	encoded, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		slog.Error("api: encoding XMLTV guide", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	body := append([]byte(xml.Header), encoded...)
	body = append(body, '\n')
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "private, max-age=300")
	_, _ = w.Write(body)
}

func formatXMLTVTime(value time.Time) string {
	return value.Format("20060102150405 -0700")
}

func programAPIURL(site string, programID int64) string {
	return fmt.Sprintf("/api/sites/%s/programs/%s", url.PathEscape(site), strconv.FormatInt(programID, 10))
}
