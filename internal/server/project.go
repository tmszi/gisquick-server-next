package server

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io/ioutil"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/gisquick/gisquick-server/internal/domain"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

func getProjectName(c echo.Context) string {
	user := c.Param("user")
	name := c.Param("name")
	return filepath.Join(user, name)
}

func (s *Server) handleGetProject(c echo.Context) error {
	projectName := getProjectName(c)
	info, err := s.projects.GetProjectInfo(projectName)
	if err != nil {
		if errors.Is(err, domain.ErrProjectNotExists) {
			s.log.Errorw(err.Error(), "handler", "handleGetProject")
			s.log.Errorw("handleGetProject", zap.Error(err))
			return echo.ErrNotFound
		}
		return err
	}
	if info.State != "published" {
		return echo.NewHTTPError(http.StatusBadRequest, "Project not valid")
	}

	// if !s.checkProjectAccess(info, c) {
	// 	return echo.ErrForbidden
	// }

	user, err := s.auth.GetUser(c)
	data, err := s.projects.GetMapConfig(projectName, user)
	if err != nil {
		return err
	}
	data["status"] = 200
	// delete(data, "layers")
	// return c.JSON(http.StatusOK, data["layers"])
	return c.JSON(http.StatusOK, data)
}

func (s *Server) handleMapOws() func(c echo.Context) error {
	/*
		director := func(req *http.Request) {
			target, _ := url.Parse(s.Config.MapserverURL)
			query := req.URL.Query()
			mapParam := req.URL.Query().Get("MAP")
			query.Set("MAP", filepath.Join("/publish", mapParam))
			req.URL.RawQuery = query.Encode()
			req.URL.Path = target.Path
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host

			if _, ok := req.Header["User-Agent"]; !ok {
				// explicitly disable User-Agent so it's not set to default value
				req.Header.Set("User-Agent", "")
			}
		}
	*/
	director := func(req *http.Request) {
		target, _ := url.Parse(s.Config.MapserverURL)
		s.log.Infow("Map proxy", "query", req.URL.RawQuery)
		req.URL.Path = target.Path
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host

		if _, ok := req.Header["User-Agent"]; !ok {
			// explicitly disable User-Agent so it's not set to default value
			req.Header.Set("User-Agent", "")
		}
		req.Header.Del("Cookie")
	}
	rewriteGetCapabilities := func(resp *http.Response) (err error) {
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		err = resp.Body.Close()
		if err != nil {
			return err
		}
		// original url is still in xsi:schemaLocation
		// regexp.MustCompile(`xsi:schemaLocation="(.)+"`)

		// reg := regexp.MustCompile(`xlink:href="http://localhost[^"]+"`)
		reg := regexp.MustCompile(`xlink:href="http[s]?://[^"]+MAP=[^"]+"`)

		owsPath := resp.Request.Header.Get("X-Ows-Url")
		doc := string(body)
		replaced := make(map[string]string, 2)
		for _, match := range reg.FindAllString(doc, -1) {
			_, done := replaced[match]
			if !done {
				u := strings.TrimPrefix(match, `xlink:href="`)
				u = strings.TrimSuffix(u, `"`)
				parsed, _ := url.Parse(html.UnescapeString(u))
				params := parsed.Query()
				params.Del("MAP")
				parsed.Path = owsPath
				parsed.RawQuery = params.Encode()
				replaced[match] = fmt.Sprintf(`xlink:href="%s"`, html.EscapeString(parsed.String()))
				doc = strings.ReplaceAll(doc, match, replaced[match])
			}
		}
		newBody := []byte(doc)
		resp.Body = ioutil.NopCloser(bytes.NewReader(newBody))
		resp.ContentLength = int64(len(newBody))
		resp.Header.Set("Content-Length", strconv.Itoa(len(newBody)))
		return nil
	}
	reverseProxy := &httputil.ReverseProxy{Director: director}
	capabilitiesProxy := &httputil.ReverseProxy{Director: director}
	capabilitiesProxy.ModifyResponse = rewriteGetCapabilities

	return func(c echo.Context) error {
		params := new(OwsRequestParams)
		if err := (&echo.DefaultBinder{}).BindQueryParams(c, params); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "Invalid query parameters")
		}

		projectName := getProjectName(c)
		pInfo, err := s.projects.GetProjectInfo(projectName)
		if err != nil {
			if errors.Is(err, domain.ErrProjectNotExists) {
				return echo.ErrNotFound
			}
			s.log.Errorw("ows handler", zap.Error(err))
			return err
		}
		settings, err := s.projects.GetSettings(projectName)
		if err != nil {
			return fmt.Errorf("getting project settings: %w", err)
		}

		req := c.Request()
		// Set MAP parameter
		owsProject := filepath.Join("/publish", projectName, pInfo.QgisFile)
		query := req.URL.Query()
		query.Set("MAP", owsProject)
		req.URL.RawQuery = query.Encode()

		if params.Service == "WMS" && strings.EqualFold(params.Request, "GetCapabilities") {
			req.Header.Set("X-Ows-Url", req.URL.Path)
			capabilitiesProxy.ServeHTTP(c.Response(), req)
			return nil
		}

		user, err := s.auth.GetUser(c) // todo: load user data only when needed (access control is defined)
		// perms := settings.UserLayersPermissions(user)
		perms := make(map[string]domain.LayerPermission)

		if params.Service == "WFS" && params.Request == "" {

			layersData, err := s.projects.GetLayersData(projectName)
			if err != nil {
				return fmt.Errorf("getting layer data: %w", err)
			}
			// p, err := s.projects.GetProject(projectName)
			// if err != nil {
			// 	return err
			// }

			getLayerPermissions := func(typeName string) domain.LayerPermission {
				parts := strings.Split(typeName, ":")
				lname := parts[len(parts)-1]
				id, _ := layersData.LayerNameToID[lname]
				perm, ok := perms[id]
				if !ok {
					perm = settings.UserLayerPermissions(user, id)
					perms[id] = perm
				}
				return perm
			}

			var wfsTransaction Transaction
			// read all bytes from content body and create new stream using it.
			bodyBytes, _ := ioutil.ReadAll(req.Body)
			req.Body = ioutil.NopCloser(bytes.NewBuffer(bodyBytes))
			if err := xml.Unmarshal(bodyBytes, &wfsTransaction); err != nil {
				return err
			}
			for _, u := range wfsTransaction.Updates {
				if !getLayerPermissions(u.TypeName).Update {
					return echo.ErrForbidden
				}
			}
			for _, i := range wfsTransaction.Inserts {
				for _, o := range i.Objects {
					if !getLayerPermissions(o.XMLName.Local).Insert {
						return echo.ErrForbidden
					}
				}
			}
			for _, d := range wfsTransaction.Deletes {
				if !getLayerPermissions(d.TypeName).Delete {
					return echo.ErrForbidden
				}
			}
		}

		reverseProxy.ServeHTTP(c.Response(), req)
		return nil
	}
}

func (s *Server) handleGetLayerCapabilities() func(c echo.Context) error {
	director := func(req *http.Request) {}
	reverseProxy := &httputil.ReverseProxy{Director: director}

	return func(c echo.Context) error {
		projectName := c.Get("project").(string)
		// projectName := getProjectName(c)
		type LayersMetadata struct {
			Layers map[string]domain.LayerMeta `json:"layers"`
		}
		var meta LayersMetadata
		err := s.projects.GetQgisMetadata(projectName, &meta)
		if err != nil {
			if errors.Is(err, domain.ErrProjectNotExists) {
				s.log.Errorw(err.Error(), "handler", "handleGetProject")
				s.log.Errorw("handleGetProject", zap.Error(err))
				return echo.ErrNotFound
			}
			return err
		}
		layername := c.QueryParam("LAYER")
		if layername == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "Missing LAYER parameter")
		}
		var lmeta domain.LayerMeta

		for _, layer := range meta.Layers {
			if layer.Name == layername {
				lmeta = layer
				sourceURL := lmeta.SourceParams.String("url")
				req, err := http.NewRequest(http.MethodGet, sourceURL, nil)
				if err != nil {
					return fmt.Errorf("handleGetLayerCapabilities error: %w", err)
				}
				for k, vv := range c.Request().Header {
					if strings.HasPrefix(k, "Accept") {
						for _, v := range vv {
							req.Header.Add(k, v)
						}
					}
				}
				reverseProxy.ServeHTTP(c.Response(), req)
				return nil
			}
		}
		return echo.NewHTTPError(http.StatusBadRequest, "Unknown LAYER name")
	}
}
