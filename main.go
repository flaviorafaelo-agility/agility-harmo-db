package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	_ "modernc.org/sqlite"
)

func jsonError(w http.ResponseWriter, msg string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

type Summary struct {
	Inserted   int `json:"inserted,omitempty"`
	Updated    int `json:"updated,omitempty"`
	NotUpdated int `json:"not_updated,omitempty"`
	Deleted    int `json:"deleted,omitempty"`
	Errors     int `json:"errors"`
	Total      int `json:"total,omitempty"`
}

func openDatabase(dbname string) (*sql.DB, error) {
	dir := "./dbs"
	os.MkdirAll(dir, 0755)
	dbpath := path.Join(dir, dbname+".db")
	db, err := sql.Open("sqlite", "file:"+dbpath)
	if err != nil {
		return nil, err
	}

	// Cria fisicamente o banco executando uma operação de escrita
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS __sys_tables (
		table_name TEXT PRIMARY KEY,
		schema_json TEXT NOT NULL
	);`)
	if err != nil {
		db.Close()
		return nil, err
	}

	return db, nil
}

func validateWithSchema(schema map[string]interface{}, jsonStr string, prefix string) (bool, string) {
	for key, val := range schema {
		fullKey := key
		if prefix != "" {
			fullKey = prefix + "." + key
		}

		switch expected := val.(type) {
		case string:
			value := gjson.Get(jsonStr, fullKey)
			switch expected {
			case "string":
				if !value.Exists() || value.Type != gjson.String {
					return false, fmt.Sprintf("Field '%s' should be string", fullKey)
				}
			case "int":
				if !value.Exists() || value.Type != gjson.Number || !isInteger(value.Num) {
					return false, fmt.Sprintf("Field '%s' should be int", fullKey)
				}
			case "bool":
				if !value.Exists() || (value.Type != gjson.True && value.Type != gjson.False) {
					return false, fmt.Sprintf("Field '%s' should be bool", fullKey)
				}
			case "decimal":
				if !value.Exists() || value.Type != gjson.Number {
					return false, fmt.Sprintf("Field '%s' should be decimal", fullKey)
				}
			case "date":
				if !value.Exists() || value.Type != gjson.String {
					return false, fmt.Sprintf("Field '%s' should be date (string format)", fullKey)
				}
				_, err := time.Parse("2006-01-02", value.String())
				if err != nil {
					return false, fmt.Sprintf("Field '%s' should be a valid date in 'YYYY-MM-DD' format", fullKey)
				}
			default:
				return false, fmt.Sprintf("Unknown expected type for field '%s': %v", fullKey, expected)
			}

		case []interface{}:
			value := gjson.Get(jsonStr, fullKey)
			if !value.Exists() || !value.IsArray() {
				return false, fmt.Sprintf("Field '%s' should be array", fullKey)
			}
			for i := range value.Array() {
				if len(expected) > 0 {
					schemaEntry, ok := expected[0].(map[string]interface{})
					if !ok {
						return false, fmt.Sprintf("Invalid schema for array field '%s'", fullKey)
					}
					childPrefix := fmt.Sprintf("%s.%d", fullKey, i)
					ok, msg := validateWithSchema(schemaEntry, jsonStr, childPrefix)
					if !ok {
						return false, msg
					}
				}
			}
		default:
			return false, fmt.Sprintf("Unsupported schema format for field '%s'", fullKey)
		}
	}
	return true, ""
}

func isInteger(f float64) bool {
	return f == float64(int64(f))
}

func handleDocument(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && r.URL.Path == "/database" {
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
			jsonError(w, "Invalid JSON or missing 'name'", http.StatusBadRequest)
			return
		}
		_, err := openDatabase(req.Name)
		if err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"message": "Database created"})
		return
	}

	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/documents") {
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) < 2 {
			jsonError(w, "Invalid path", http.StatusBadRequest)
			return
		}
		dbname := parts[1]
		var req struct {
			Name   string                 `json:"name"`
			Schema map[string]interface{} `json:"schema,omitempty"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
			jsonError(w, "Invalid JSON or missing 'name'", http.StatusBadRequest)
			return
		}
		db, err := openDatabase(dbname)
		if err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer db.Close()

		// Verifica se a tabela já está registrada na __sys_tables
		var exists int
		err = db.QueryRow(`SELECT COUNT(*) FROM __sys_tables WHERE table_name = ?`, req.Name).Scan(&exists)
		if err != nil {
			jsonError(w, "Error checking existing table: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if exists > 0 {
			jsonError(w, fmt.Sprintf("Table '%s' already exists", req.Name), http.StatusBadRequest)
			return
		}

		// Cria a tabela de documentos se ainda não existir
		_, err = db.Exec(fmt.Sprintf(`CREATE TABLE IF NOT EXISTS "%s" (
			id TEXT PRIMARY KEY,
			data TEXT NOT NULL
		);`, req.Name))
		if err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// Salva o schema como JSON na tabela __sys_tables
		schemaJSON, err := json.Marshal(req.Schema)
		if err != nil {
			jsonError(w, "Error serializing schema: "+err.Error(), http.StatusInternalServerError)
			return
		}

		_, err = db.Exec(`
			INSERT OR REPLACE INTO __sys_tables (table_name, schema_json)
			VALUES (?, ?)`, req.Name, string(schemaJSON))
		if err != nil {
			jsonError(w, "Error saving schema: "+err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"message": "Document created with schema"})
		return
	}

	jsonError(w, "Invalid route or method", http.StatusBadRequest)
}

func parseFilter(key, raw string) (string, interface{}) {
	op := "="
	r := regexp.MustCompile(`^(<=|>=|!=|=|<|>)(.*)$`)
	matches := r.FindStringSubmatch(raw)
	if len(matches) == 3 {
		op = matches[1]
		raw = matches[2]
	}

	var param interface{} = raw
	if i, err := strconv.Atoi(raw); err == nil {
		param = i
	} else if b, err := strconv.ParseBool(raw); err == nil {
		param = b
	}

	return fmt.Sprintf("json_extract(data, '$.%s') %s ?", key, op), param
}

func listDatabases(w http.ResponseWriter, r *http.Request) {
	dir := "./dbs"
	entries, err := os.ReadDir(dir)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	dbs := []string{}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".db") {
			dbs = append(dbs, strings.TrimSuffix(entry.Name(), ".db"))
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"databases": dbs})
}

func listDocuments(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 2 {
		jsonError(w, "Invalid path", http.StatusBadRequest)
		return
	}
	dbname := parts[0]
	db, err := openDatabase(dbname)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer db.Close()

	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type='table'")
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	documents := []map[string]interface{}{}
	for rows.Next() {
		var name string
		rows.Scan(&name)
		var count int
		row := db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", name))
		row.Scan(&count)
		documents = append(documents, map[string]interface{}{"name": name, "count": count})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"documents": documents})
}

func diffFields(prefix string, a, b interface{}) []string {
	var diffs []string

	switch aVal := a.(type) {
	case map[string]interface{}:
		bMap, ok := b.(map[string]interface{})
		if !ok {
			return []string{prefix}
		}
		for key, aItem := range aVal {
			subPath := key
			if prefix != "" {
				subPath = prefix + "." + key
			}
			diffs = append(diffs, diffFields(subPath, aItem, bMap[key])...)
		}
	case []interface{}:
		bArr, ok := b.([]interface{})
		if !ok {
			return []string{prefix}
		}
		max := len(aVal)
		if len(bArr) > max {
			max = len(bArr)
		}
		for i := 0; i < max; i++ {
			var aItem, bItem interface{}
			if i < len(aVal) {
				aItem = aVal[i]
			}
			if i < len(bArr) {
				bItem = bArr[i]
			}
			subPath := fmt.Sprintf("%s[%d]", prefix, i)
			diffs = append(diffs, diffFields(subPath, aItem, bItem)...)
		}
	default:
		if !reflect.DeepEqual(a, b) {
			diffs = append(diffs, prefix)
		}
	}
	return diffs
}

func genericHandler(w http.ResponseWriter, r *http.Request) {
	dbname := r.URL.Query().Get("db")
	doc := r.URL.Query().Get("doc")
	if dbname == "" || doc == "" {
		jsonError(w, "Missing 'db' or 'doc' in query parameters", http.StatusBadRequest)
		return
	}
	db, err := openDatabase(dbname)
	if err != nil {
		err := fmt.Errorf("database '%s' not found", dbname)
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer db.Close()

	switch r.Method {
	case http.MethodGet:
		id := r.URL.Query().Get("id")
		if id != "" {
			var raw string
			err := db.QueryRow(fmt.Sprintf("SELECT data FROM %s WHERE id = ?", doc), id).Scan(&raw)
			if err == sql.ErrNoRows {
				jsonError(w, "Record not found", http.StatusNotFound)
				return
			} else if err != nil {
				jsonError(w, err.Error(), http.StatusInternalServerError)
				return
			}
			var item map[string]interface{}
			json.Unmarshal([]byte(raw), &item)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(item)
			return
		}
		filters := r.URL.Query()
		conditions := []string{}
		params := []interface{}{}
		for key, values := range filters {
			if key == "db" || key == "doc" || key == "id" {
				continue
			}
			for _, val := range values {
				cond, param := parseFilter(key, val)
				conditions = append(conditions, cond)
				params = append(params, param)
			}
		}
		where := strings.Join(conditions, " AND ")
		query := fmt.Sprintf("SELECT data FROM %s", doc)
		if where != "" {
			query += " WHERE " + where
		}
		rows, err := db.Query(query, params...)
		if err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		results := []map[string]interface{}{} // inicializa vazio para evitar null
		for rows.Next() {
			var raw string
			rows.Scan(&raw)
			var item map[string]interface{}
			json.Unmarshal([]byte(raw), &item)
			results = append(results, item)
		}
		if results == nil {
			results = []map[string]interface{}{} // garante array vazio, não null
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"summary": map[string]int{"total": len(results)},
			"data":    results,
		})
	case http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		var dataList []map[string]interface{}
		json.Unmarshal(body, &dataList)

		var schemaRaw string
		db.QueryRow("SELECT schema_json FROM __sys_tables WHERE table_name = ?", doc).Scan(&schemaRaw)
		schema := map[string]interface{}{}
		json.Unmarshal([]byte(schemaRaw), &schema)

		tx, _ := db.Begin()
		stmt, _ := tx.Prepare(fmt.Sprintf("INSERT INTO %s(id, data) VALUES(?, ?)", doc))
		defer stmt.Close()
		var inserted, errors []map[string]interface{}
		for _, item := range dataList {
			if _, exists := item["_id"]; exists {
				errors = append(errors, map[string]interface{}{"error": "'_id' must not be sent", "record": item})
				continue
			}
			uid := uuid.New().String()
			item["_id"] = uid
			jsonData, _ := json.Marshal(item)

			ok, msg := validateWithSchema(schema, string(jsonData), "")
			if !ok {
				errors = append(errors, map[string]interface{}{"error": msg, "record": item})
				continue
			}

			if _, err := stmt.Exec(uid, string(jsonData)); err != nil {
				errors = append(errors, map[string]interface{}{"error": err.Error(), "record": item})
				continue
			}
			inserted = append(inserted, item)
		}
		tx.Commit()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"summary": map[string]int{
				"inserted": len(inserted),
				"errors":   len(errors),
			},
			"inserted": inserted,
			"errors":   errors,
		})
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		var dataList []map[string]interface{}
		json.Unmarshal(body, &dataList)

		var schemaRaw string
		db.QueryRow("SELECT schema_json FROM __sys_tables WHERE table_name = ?", doc).Scan(&schemaRaw)
		schema := map[string]interface{}{}
		json.Unmarshal([]byte(schemaRaw), &schema)

		tx, _ := db.Begin()
		stmt, _ := tx.Prepare(fmt.Sprintf("UPDATE %s SET data = ? WHERE id = ?", doc))
		defer stmt.Close()
		var updated, notUpdated, errors []map[string]interface{}
		for _, item := range dataList {
			id, ok := item["_id"].(string)
			if !ok || id == "" {
				errors = append(errors, map[string]interface{}{"error": "Missing or invalid '_id'", "record": item})
				continue
			}

			// Buscar valor atual no banco
			var currentRaw string
			err := tx.QueryRow(fmt.Sprintf("SELECT data FROM %s WHERE id = ?", doc), id).Scan(&currentRaw)
			if err == sql.ErrNoRows {
				errors = append(errors, map[string]interface{}{"error": "Record not found", "record": item})
				continue
			}

			var current map[string]interface{}
			json.Unmarshal([]byte(currentRaw), &current)

			// Validar novo item com schema
			jsonData, _ := json.Marshal(item)
			ok, msg := validateWithSchema(schema, string(jsonData), "")
			if !ok {
				errors = append(errors, map[string]interface{}{"error": msg, "record": item})
				continue
			}

			changedFields := diffFields("", current, item)

			if len(changedFields) > 0 {
				item["_changed"] = changedFields
				stmt.Exec(string(jsonData), id)
				updated = append(updated, item)
			} else {
				notUpdated = append(notUpdated, item)
			}
		}
		tx.Commit()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"_summary": map[string]int{
				"updated":     len(updated),
				"not_updated": len(notUpdated),
				"errors":      len(errors),
			},
			"_updated":     updated,
			"_not_updated": notUpdated,
			"_errors":      errors,
		})

	case http.MethodDelete:
		body, _ := io.ReadAll(r.Body)
		var dataList []map[string]interface{}
		json.Unmarshal(body, &dataList)
		stmt, _ := db.Prepare(fmt.Sprintf("DELETE FROM %s WHERE id = ?", doc))
		defer stmt.Close()
		var deleted []string
		var errors []map[string]interface{}
		for _, item := range dataList {
			id, ok := item["_id"].(string)
			if !ok || id == "" {
				errors = append(errors, map[string]interface{}{"error": "Missing or invalid '_id'", "record": item})
				continue
			}
			res, err := stmt.Exec(id)
			if err != nil {
				errors = append(errors, map[string]interface{}{"error": err.Error(), "record": item})
				continue
			}
			if n, _ := res.RowsAffected(); n == 0 {
				errors = append(errors, map[string]interface{}{"error": "Record not found", "record": item})
				continue
			}
			deleted = append(deleted, id)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"summary": map[string]int{
				"deleted": len(deleted),
				"errors":  len(errors),
			},
			"deleted": deleted,
			"errors":  errors,
		})
	default:
		jsonError(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func listDocsSchema(w http.ResponseWriter, r *http.Request) {
	dbname := strings.TrimPrefix(r.URL.Path, "/docs/")
	if dbname == "" {
		jsonError(w, "Missing database name", http.StatusBadRequest)
		return
	}
	db, err := openDatabase(dbname)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer db.Close()

	rows, err := db.Query(`SELECT table_name, schema_json FROM __sys_tables`)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	schemas := make(map[string]interface{})
	for rows.Next() {
		var name string
		var raw string
		err := rows.Scan(&name, &raw)
		if err != nil {
			continue
		}
		var schema interface{}
		json.Unmarshal([]byte(raw), &schema)
		schemas[name] = schema
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(schemas)
}

func swaggerUIDoc(w http.ResponseWriter, r *http.Request) {
	dbsDir := "./dbs"
	entries, err := os.ReadDir(dbsDir)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	paths := make(map[string]interface{})
	schemas := make(map[string]interface{})
	tags := []map[string]string{}

	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".db") {
			dbname := strings.TrimSuffix(entry.Name(), ".db")
			tags = append(tags, map[string]string{
				"name":        dbname,
				"description": fmt.Sprintf("Operações para o banco %s", dbname),
			})

			db, err := openDatabase(dbname)
			if err != nil {
				log.Printf("Erro ao abrir %s: %v", dbname, err)
				continue
			}

			rows, err := db.Query(`SELECT table_name, schema_json FROM __sys_tables`)
			if err != nil {
				log.Printf("Erro ao consultar __sys_tables de %s: %v", dbname, err)
				db.Close()
				continue
			}

			for rows.Next() {
				var table, raw string
				if err := rows.Scan(&table, &raw); err != nil {
					log.Printf("Erro ao fazer Scan em %s: %v", table, err)
					continue
				}

				var properties map[string]interface{}
				if err := json.Unmarshal([]byte(raw), &properties); err != nil {
					log.Printf("Erro ao fazer Unmarshal do schema de %s.%s: %v", dbname, table, err)
					continue
				}

				schemaName := fmt.Sprintf("%s_%s", dbname, table)
				schemas[schemaName] = map[string]interface{}{
					"type":       "object",
					"properties": properties,
				}

				path := fmt.Sprintf("/%s/%s", dbname, table)
				paths[path] = map[string]interface{}{
					"tags": []string{dbname},
					"get": map[string]interface{}{
						"summary":     fmt.Sprintf("Listar registros de %s.%s", dbname, table),
						"operationId": fmt.Sprintf("list_%s_%s", dbname, table),
						"responses": map[string]interface{}{
							"200": map[string]interface{}{
								"description": "Operação bem-sucedida",
								"content": map[string]interface{}{
									"application/json": map[string]interface{}{
										"schema": map[string]interface{}{
											"type": "array",
											"items": map[string]interface{}{
												"$ref": "#/components/schemas/" + schemaName,
											},
										},
									},
								},
							},
						},
					},
					"post": map[string]interface{}{
						"summary":     fmt.Sprintf("Inserir registros em %s.%s", dbname, table),
						"operationId": fmt.Sprintf("insert_%s_%s", dbname, table),
						"requestBody": map[string]interface{}{
							"required": true,
							"content": map[string]interface{}{
								"application/json": map[string]interface{}{
									"schema": map[string]interface{}{
										"type":  "array",
										"items": map[string]interface{}{"$ref": "#/components/schemas/" + schemaName},
									},
								},
							},
						},
						"responses": map[string]interface{}{
							"200": map[string]interface{}{
								"description": "Inserção realizada com sucesso",
							},
						},
					},
					"put": map[string]interface{}{
						"summary":     fmt.Sprintf("Atualizar registros em %s.%s", dbname, table),
						"operationId": fmt.Sprintf("update_%s_%s", dbname, table),
						"requestBody": map[string]interface{}{
							"required": true,
							"content": map[string]interface{}{
								"application/json": map[string]interface{}{
									"schema": map[string]interface{}{
										"type":  "array",
										"items": map[string]interface{}{"$ref": "#/components/schemas/" + schemaName},
									},
								},
							},
						},
						"responses": map[string]interface{}{
							"200": map[string]interface{}{
								"description": "Atualização realizada com sucesso",
							},
						},
					},
					"delete": map[string]interface{}{
						"summary":     fmt.Sprintf("Excluir registros de %s.%s", dbname, table),
						"operationId": fmt.Sprintf("delete_%s_%s", dbname, table),
						"requestBody": map[string]interface{}{
							"required": true,
							"content": map[string]interface{}{
								"application/json": map[string]interface{}{
									"schema": map[string]interface{}{
										"type":  "array",
										"items": map[string]interface{}{"$ref": "#/components/schemas/" + schemaName},
									},
								},
							},
						},
						"responses": map[string]interface{}{
							"200": map[string]interface{}{
								"description": "Exclusão realizada com sucesso",
							},
						},
					},
				}
			}

			rows.Close()
			db.Close()
		}
	}

	swagger := map[string]interface{}{
		"openapi": "3.0.0",
		"info": map[string]interface{}{
			"title":   "HarmoDB Dinâmico",
			"version": "1.0.0",
		},
		"tags":  tags,
		"paths": paths,
		"components": map[string]interface{}{
			"schemas": schemas,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(swagger)
}

func swaggerUIHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html>
  <head>
    <title>Swagger UI</title>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist/swagger-ui.css" />
  </head>
  <body>
    <div id="swagger-ui"></div>
    <script src="https://unpkg.com/swagger-ui-dist/swagger-ui-bundle.js"></script>
    <script>
      window.onload = function() {
        const url = new URLSearchParams(window.location.search).get("url") || "/swagger.json";
        SwaggerUIBundle({
          url: url,
          dom_id: '#swagger-ui',
        });
      }
    </script>
  </body>
</html>`)
}

func main() {
	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("./static"))))

	http.HandleFunc("/database", handleDocument)
	http.HandleFunc("/databases", listDatabases)

	http.HandleFunc("/swagger.json", swaggerUIDoc)
	http.HandleFunc("/swagger", swaggerUIHandler)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")

		// POST /database
		if r.URL.Path == "database" && r.Method == http.MethodPost {
			handleDocument(w, r)
			return
		}

		if len(parts) == 2 && parts[1] == "documents" && r.Method == http.MethodGet {
			listDocuments(w, r)
			return
		}

		// POST /{db}/documents
		if len(parts) == 2 && r.Method == http.MethodPost && parts[1] == "documents" {
			handleDocument(w, r)
			return
		}

		// GET|POST|PUT|DELETE /{db}/{doc}
		if len(parts) == 2 {
			dbname := parts[0]
			doc := parts[1]
			r.URL.RawQuery = r.URL.RawQuery + "&db=" + dbname + "&doc=" + doc
			genericHandler(w, r)
			return
		}

		http.NotFound(w, r)
	})

	fmt.Println("Server running at http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
