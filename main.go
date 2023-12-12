package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"

	_ "github.com/mattn/go-sqlite3"
	"github.com/quantonganh/geohash"
)

type handler func(w http.ResponseWriter, r *http.Request)

func nearby(db *sql.DB) handler {
	return func(w http.ResponseWriter, r *http.Request) {
		latStr := r.URL.Query().Get("latitude")
		lat, err := strconv.ParseFloat(latStr, 64)
		if err != nil {
			http.Error(w, "Invalid latitude parameter", http.StatusBadRequest)
			return
		}

		lngStr := r.URL.Query().Get("longitude")
		lng, err := strconv.ParseFloat(lngStr, 64)
		if err != nil {
			http.Error(w, "Invalid longitude parameter", http.StatusBadRequest)
			return
		}

		radiusStr := r.URL.Query().Get("radius")
		radius, err := strconv.ParseFloat(radiusStr, 64)
		if err != nil {
			http.Error(w, "Invalid radius parameter", http.StatusBadRequest)
			return
		}

		minLat, maxLat, minLng, maxLng := geohash.BoundingBox(lat, lng, radius)
		swGeohash := geohash.Encode(minLat, minLng)
		neGeohash := geohash.Encode(maxLat, maxLng)

		rows, err := db.Query(`
			SELECT c.city, c.lat, c.lng, c.country, g.geohash
			FROM cities c JOIN geospatial_index g ON g.city_id = c.id
			WHERE g.geohash BETWEEN ? and ?;
		`, swGeohash, neGeohash)
		if err != nil {
			http.Error(w, "Database query error", http.StatusInternalServerError)
			return
		}

		type city struct {
			Name      string  `json:"name"`
			Latitude  float64 `json:"lat"`
			Longitude float64 `json:"lng"`
			Country   string  `json:"country"`
			Geohash   string  `json:"geohash"`
		}
		cities := make([]city, 0)
		for rows.Next() {
			var c city
			if err := rows.Scan(&c.Name, &c.Latitude, &c.Longitude, &c.Country, &c.Geohash); err != nil {
				http.Error(w, "Error scanning database rows", http.StatusInternalServerError)
				return
			}
			cities = append(cities, c)
		}

		data, err := json.Marshal(cities)
		if err != nil {
			http.Error(w, "Error marshaling JSON response", http.StatusInternalServerError)
			return
		}

		w.Write(data)
	}
}

func main() {
	db, err := sql.Open("sqlite3", "nearby_cities.db")
	if err != nil {
		log.Fatal(err)
	}

	if err := createSpatialIndex(db); err != nil {
		log.Fatal(err)
	}

	http.HandleFunc("/v1/cities/nearby", nearby(db))

	server := &http.Server{
		Addr: ":8080",
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

	if !migrationApplied {
		cmd := exec.Command("sqlite3", "nearby_cities.db", "-cmd", ".mode csv", ".import worldcities.csv cities")
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
