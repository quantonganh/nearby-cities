package main

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"sort"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	_ "github.com/mattn/go-sqlite3"
	"github.com/quantonganh/geohash"
	"github.com/quantonganh/httperror"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"
)

//go:embed templates/*.html
var htmlFS embed.FS

//go:embed static
var staticFS embed.FS

//go:embed worldcities.csv
var worldCitiesCSV string

func main() {
	db, err := sql.Open("sqlite3", "nearby_cities.db")
	if err != nil {
		log.Fatal(err)
	}

	if err := createSpatialIndex(db); err != nil {
		log.Fatal(err)
	}

	zlog := zerolog.New(os.Stdout).With().
		Timestamp().
		Logger()

	r := mux.NewRouter()
	r.Use(hlog.NewHandler(zlog))
	r.Use(hlog.AccessHandler(func(r *http.Request, status, size int, duration time.Duration) {
		hlog.FromRequest(r).Info().
			Str("method", r.Method).
			Stringer("url", r.URL).
			Int("status", status).
			Int("size", size).
			Dur("duration", duration).
			Msg("")
	}))
	r.Use(hlog.UserAgentHandler("user_agent"))
	r.Use(hlog.RefererHandler("referer"))
	r.Use(hlog.RequestIDHandler("req_id", "Request-Id"))

	tmpl, err := template.New("index.html").ParseFS(htmlFS, "templates/*.html")
	if err != nil {
		log.Fatal(err)
	}

	r.PathPrefix("/static/").Handler(http.FileServer(http.FS(staticFS)))
	r.Handle("/", errorHandler(indexHandler(tmpl)))
	r.Handle("/search", errorHandler(searchHandler(db, tmpl)))

	server := &http.Server{
		Addr:    ":8080",
		Handler: r,
	}
	go func() {
		fmt.Printf("Server is listening on port %s...\n", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	fmt.Println("\nShutting down server...")
	if err := server.Shutdown(context.Background()); err != nil {
		log.Fatal(err)
	}

	if err := db.Close(); err != nil {
		log.Fatal(err)
	}

	fmt.Println("Server has stopped.")
}

func createSpatialIndex(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS migrations (name TEXT PRIMARY KEY);`); err != nil {
		return fmt.Errorf("error creating migrations table: %w", err)
	}

	var migrationApplied bool
	err := db.QueryRow(`
		SELECT EXISTS (SELECT 1 from migrations WHERE name = 'cities_table')
	`).Scan(&migrationApplied)
	if err != nil {
		return fmt.Errorf("error checking migration status: %w", err)
	}

	tmpFile, err := ioutil.TempFile("", "worldcities*.csv")
	if err != nil {
		return fmt.Errorf("error creating temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.Write([]byte(worldCitiesCSV)); err != nil {
		return fmt.Errorf("error writing the embedded CSV content: %w", err)
	}

	if !migrationApplied {
		cmd := exec.Command("sqlite3", "nearby_cities.db", "-cmd", ".mode csv", fmt.Sprintf(".import %s cities", tmpFile.Name()))
		_, err = cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("error importing CSV data into cities table: %w", err)
		}

		_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS geospatial_index (
			geohash TEXT,
			city_id INTEGER UNIQUE,
			FOREIGN KEY(city_id) REFERENCES cities(id)
		)
		`)
		if err != nil {
			return fmt.Errorf("error creating goepatial_index table: %w", err)
		}

		_, err = db.Exec(`
			CREATE VIRTUAL TABLE cities_fts USING fts5(
				city,
				city_ascii,
				lat,
				lng,
				country,
				iso2,
				iso3,
				admin_name,
				capital,
				population,
				id,
				content='cities',
				tokenize='unicode61'
			);
		`)
		if err != nil {
			return fmt.Errorf("error creating cities_fts table: %w", err)
		}

		_, err = db.Exec(`
			INSERT INTO cities_fts(city, city_ascii, lat, lng, country, iso2, iso3, admin_name, capital, population, id)
			SELECT city, city_ascii, lat, lng, country, iso2, iso3, admin_name, capital, population, id FROM cities;
		`)
		if err != nil {
			return fmt.Errorf("error populating the virtual table cities_fts: %w", err)
		}

		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("error starting transaction: %w", err)
		}
		defer tx.Rollback()

		rows, err := tx.Query(`
		SELECT id, lat, lng FROM cities
		`)
		if err != nil {
			return fmt.Errorf("error selecting lat, lng from cities table: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var (
				id       int
				lat, lng float64
			)
			if err := rows.Scan(&id, &lat, &lng); err != nil {
				return fmt.Errorf("error scanning: %w", err)
			}

			gh := geohash.Encode(lat, lng)

			_, err = tx.Exec(`
			INSERT INTO geospatial_index (geohash, city_id)
			VALUES (?, ?)
		`, gh, id)
			if err != nil {
				return fmt.Errorf("error inserting into geospatial_index: %w", err)
			}
		}

		if err := rows.Err(); err != nil {
			return fmt.Errorf("error during iteration: %w", err)
		}

		_, err = tx.Exec("INSERT INTO migrations (name) VALUES ('cities_table')")
		if err != nil {
			return fmt.Errorf("error marking migration as applied: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("error committing transaction: %w", err)
		}
	}

	return nil
}

type PageData struct {
	City    string
	Radius  string
	Cities  []city
	Message string
}

type city struct {
	City       string
	CityAscii  string
	Lat        float64
	Lng        float64
	Country    string
	Iso2       string
	Iso3       string
	AdminName  string
	Capital    string
	Population string
	ID         string
	Geohash    string
	Distance   float64
}

func indexHandler(tmpl *template.Template) httperror.Handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		return tmpl.ExecuteTemplate(w, "base", PageData{})
	}
}

func searchHandler(db *sql.DB, tmpl *template.Template) httperror.Handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		cityQuery := r.FormValue("city")
		normalizedCity := normalizeQuery(cityQuery)

		row := db.QueryRow(`
			SELECT city, lat, lng, country FROM cities_fts WHERE cities_fts MATCH ? 
			`, normalizedCity)
		var fromCity city
		err := row.Scan(&fromCity.City, &fromCity.Lat, &fromCity.Lng, &fromCity.Country)
		if err != nil {
			if err == sql.ErrNoRows {
				data := PageData{
					Message: "No matching city found.",
				}

				return tmpl.ExecuteTemplate(w, "base", data)
			} else {
				return err
			}
		}

		hash := geohash.Encode(fromCity.Lat, fromCity.Lng)
		length := geohash.EstimateLengthRequired(100)
		rows, err := db.Query(`
			SELECT c.city, c.lat, c.lng, c.admin_name, c.country, g.geohash
			FROM cities c JOIN geospatial_index g ON g.city_id = c.id
			WHERE g.geohash LIKE ?;
		`, fmt.Sprintf("%s%%", hash[:length]))
		if err != nil {
			return err
		}

		cities := make([]city, 0)
		for rows.Next() {
			var toCity city
			if err := rows.Scan(&toCity.City, &toCity.Lat, &toCity.Lng, &toCity.AdminName, &toCity.Country, &toCity.Geohash); err != nil {
				return err
			}

			distance := geohash.Distance(fromCity.Lat, fromCity.Lng, toCity.Lat, toCity.Lng)
			toCity.Distance = math.Round(distance*100) / 100
			cities = append(cities, toCity)
		}

		sort.Slice(cities, func(i, j int) bool {
			return cities[i].Distance < cities[j].Distance
		})

		data := PageData{
			City:   cityQuery,
			Cities: cities,
		}

		return tmpl.ExecuteTemplate(w, "base", data)
	}
}

func normalizeQuery(query string) string {
	re := regexp.MustCompile(`[\p{P}]`)
	return re.ReplaceAllString(query, "")
}

func errorHandler(handler httperror.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		err := handler(w, r)
		if err != nil {
			// Handle the error and send an appropriate response
			fmt.Println("Error:", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}
