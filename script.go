package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"
)

var SGDB_API_KEY string

const SGDB_API_URL = "https://www.steamgriddb.com/api/v2/"
const SGDB_COVER_FORMAT = "600x900"
const SGDB_COVER_WIDTH = 600
const SGDB_BANNER_FORMAT = "920x430"
const SGDB_BANNER_WIDTH = 920
const MIME_TYPE_JPEG = "image/jpeg"
const MIME_TYPE_PNG = "image/png"

func main() {
	err := godotenv.Load()
	SGDB_API_KEY = os.Getenv("SGDB_API_KEY")

	lutrisDirs, err := get_lutris_dir()
	if err != nil {
		log.Fatalln(err)
	}

	db, err := connect_to_lutris_db(lutrisDirs.DbFilePath)
	if err != nil {
		log.Fatalln(err)
	}
	defer db.Close()

	slugs, err := select_game_slugs(db)
	if err != nil {
		log.Fatalln(err)
	}
	slugs = filter_game_slugs_with_missing_assets(lutrisDirs, slugs)

	for _, slug := range slugs {
		id, err := fetch_steamgriddb_game_id(slug)
		if err != nil {
			continue
		}
		grids, err := fetch_steamgriddb_grids(id)
		if err != nil {
			continue
		}
		download_asset_if_needed(lutrisDirs.CoverArtDirPath, slug, SGDB_COVER_WIDTH, grids)
		download_asset_if_needed(lutrisDirs.BannersDirPath, slug, SGDB_BANNER_WIDTH, grids)
	}
}

func get_lutris_dir() (lutrisDirs, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return lutrisDirs{}, err
	}
	lutrisDir := filepath.Join(homeDir, ".local", "share", "lutris")
	return lutrisDirs{
		DbFilePath:      filepath.Join(lutrisDir, "pga.db"),
		BannersDirPath:  filepath.Join(lutrisDir, "banners"),
		CoverArtDirPath: filepath.Join(lutrisDir, "coverart"),
	}, nil
}

type lutrisDirs struct {
	DbFilePath      string
	BannersDirPath  string
	CoverArtDirPath string
}

func connect_to_lutris_db(path string) (*sql.DB, error) {
	return sql.Open("sqlite3", path)
}

func select_game_slugs(db *sql.DB) ([]string, error) {
	var slugs []string
	rows, err := db.Query("SELECT slug FROM games")
	if err != nil {
		return slugs, err
	}
	for rows.Next() {
		var slug string
		rows.Scan(&slug)
		if slug != "" {
			slugs = append(slugs, slug)
		}
	}
	return slugs, nil
}

func filter_game_slugs_with_missing_assets(dirs lutrisDirs, slugs []string) []string {
	var filtered []string
	for _, slug := range slugs {
		if assets_missing(dirs.CoverArtDirPath, slug) || assets_missing(dirs.BannersDirPath, slug) {
			filtered = append(filtered, slug)
		}
	}
	return filtered
}

func assets_missing(assetDir, slug string) bool {
	if _, err := os.Stat(filepath.Join(assetDir, fmt.Sprint(slug, ".jpg"))); err != nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(assetDir, fmt.Sprint(slug, ".png"))); err != nil {
		return true
	}
	return false
}

func fetch_steamgriddb_game_id(slug string) (int, error) {
	u, err := url.Parse(SGDB_API_URL)
	if err != nil {
		return 0, err
	}
	u.Path = path.Join(u.Path, "search/autocomplete", slug)
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	req.Header.Add("Authorization", "Bearer "+SGDB_API_KEY)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	var searchResp searchResponse
	err = json.Unmarshal(body, &searchResp)
	if err != nil {
		return 0, err
	}
	if len(searchResp.Games) == 0 {
		return 0, errors.New("no game found")
	}
	return searchResp.Games[0].Id, nil
}

type searchResponse struct {
	Games []gameData `json:"data"`
}

type gameData struct {
	Id   int    `json:"id"`
	Name string `json:"name"`
}

func fetch_steamgriddb_grids(gameId int) ([]grid, error) {
	u, err := url.Parse(SGDB_API_URL)
	if err != nil {
		return []grid{}, err
	}
	u.Path = path.Join(u.Path, "grids/game", fmt.Sprint(gameId))
	params := url.Values{}
	params.Set("dimensions", strings.Join([]string{SGDB_COVER_FORMAT, SGDB_BANNER_FORMAT}, ","))
	params.Set("types", "static")
	u.RawQuery = params.Encode()
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	req.Header.Add("Authorization", "Bearer "+SGDB_API_KEY)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return []grid{}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return []grid{}, err
	}
	var gridsResp gridsResponse
	err = json.Unmarshal(body, &gridsResp)
	if err != nil {
		return []grid{}, err
	}
	if len(gridsResp.Grids) == 0 {
		return []grid{}, errors.New("no grid found")
	}
	return gridsResp.Grids, nil
}

type gridsResponse struct {
	Grids []grid `json:"data"`
}

type grid struct {
	Url    string `json:"url"`
	Mime   string `json:"mime"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

func download_asset_if_needed(assetDir, slug string, expectedWidth int, grids []grid) error {
	if !assets_missing(assetDir, slug) {
		return errors.New("asset already exists")
	}
	var matching *grid
	for _, grid := range grids {
		if grid.Width == expectedWidth {
			matching = &grid
			break
		}
	}
	if matching == nil {
		return errors.New("no grid found with the expected format")
	}

	var ext string
	switch matching.Mime {
	case MIME_TYPE_JPEG:
		ext = ".jpg"
	case MIME_TYPE_PNG:
		ext = ".png"
	default:
		return errors.New("unexpected mime type")
	}
	out, err := os.Create(filepath.Join(assetDir, fmt.Sprint(slug, ext)))
	if err != nil {
		return err
	}
	defer out.Close()

	resp, err := http.Get(matching.Url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return err
	}
	return nil
}
