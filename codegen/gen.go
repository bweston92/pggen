package codegen

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v4"
	"github.com/jschaf/sqld/errs"
	"github.com/jschaf/sqld/internal/ast"
	"github.com/jschaf/sqld/internal/parser"
	_ "github.com/jschaf/sqld/statik"
	"github.com/rakyll/statik/fs"
	gotok "go/token"
	"html/template"
	"io/ioutil"
	"os"
	"path/filepath"
)

// GenerateOptions are the unparsed options that controls the generated Go code.
type GenerateOptions struct {
	// The connection string to the running Postgres database to use to get type
	// information for each query in QueryFiles.
	//
	// Must be parseable by pgconn.ParseConfig, like:
	//
	//   # Example DSN
	//   user=jack password=secret host=pg.example.com port=5432 dbname=foo_db sslmode=verify-ca
	//
	//   # Example URL
	//   postgres://jack:secret@pg.example.com:5432/foo_db?sslmode=verify-ca
	ConnString string
	// Generate code for each of the SQL query file paths.
	QueryFiles []string
	// The overall config after merging config files and flag options.
	Config Config
	// The name of the Go package for the file. If empty, defaults to the
	// directory name.
	GoPackage string
	// Directory to write generated files. Writes one file for each query file as
	// well as querier.go.
	OutputDir string
}

// Config is the parsed configuration.
type Config struct {
}

// merge merges this config with a new config.
func (c Config) merge(new Config) Config {
	return c
}

// Generate generates Go code to safely wrap each SQL TemplateQuery in
// opts.QueryFiles into a callable methods.
//
// Generate must only be called once per output directory.
func Generate(opts GenerateOptions) error {
	pgConnConfig, err := pgx.ParseConfig(opts.ConnString)
	if err != nil {
		return fmt.Errorf("parse postgres conn string: %w", err)
	}

	pgConn, err := pgx.ConnectConfig(context.TODO(), pgConnConfig)
	if err != nil {
		return fmt.Errorf("connect to pggen postgres database: %w", err)
	}
	queries, err := parseQueryFiles(pgConn, opts, opts.Config, opts.QueryFiles)

	if err := emitAll(opts.OutputDir, queries); err != nil {
		return fmt.Errorf("emit generated code: %w", err)
	}

	return nil
}

// mergeConfigs parses and merges all the configs using "last write wins" to
// resolve conflicts.
func mergeConfigs(configs []string) (Config, error) {
	conf := Config{}
	for _, config := range configs {
		bs, err := ioutil.ReadFile(config)
		if err != nil {
			return Config{}, fmt.Errorf("read pggen config file: %w", err)
		}
		c, err := parseConfig(bs)
		if err != nil {
			return Config{}, fmt.Errorf("parse pggen config file: %w", err)
		}
		conf = conf.merge(c)
	}
	return conf, nil
}

func parseConfig(bs []byte) (Config, error) {
	return Config{}, nil
}

// queryFile represents all of the SQL queries from a single file.
type queryFile struct {
	GoPkg           string               // the name of the Go package for the file
	Src             string               // the source file
	TemplateQueries []*ast.TemplateQuery // the queries as they appeared in the source file
	TypedQueries    []typedQuery         // the queries after inferring type information
}

func parseQueryFiles(conn *pgx.Conn, opts GenerateOptions, config Config, queryFiles []string) ([]queryFile, error) {
	files := make([]queryFile, len(queryFiles))
	pkgName := opts.GoPackage
	if opts.GoPackage == "" {
		pkgName = filepath.Base(opts.OutputDir)
	}
	for i, file := range queryFiles {
		queryFile, err := parseTemplateQueries(pkgName, file)
		if err != nil {
			return nil, fmt.Errorf("parse template query file %q: %w", file, err)
		}
		files[i] = queryFile
	}
	return files, nil
}

func parseTemplateQueries(pkgName string, file string) (queryFile, error) {
	astFile, err := parser.ParseFile(gotok.NewFileSet(), file, nil, 0)
	if err != nil {
		return queryFile{}, fmt.Errorf("parse query file %q: %w", file, err)
	}
	tmplQueries := make([]*ast.TemplateQuery, 0, len(astFile.Queries))
	for _, query := range astFile.Queries {
		switch q := query.(type) {
		case *ast.BadQuery:
			return queryFile{}, errors.New("parsed bad query instead of erroring")
		case *ast.TemplateQuery:
			tmplQueries = append(tmplQueries, q)
		default:
			return queryFile{}, fmt.Errorf("unhandled query ast type: %T", q)
		}
	}
	return queryFile{
		Src:             file,
		GoPkg:           pkgName,
		TemplateQueries: tmplQueries,
		TypedQueries:    nil,
	}, nil

}

type param struct {
	// Name of the param, like 'FirstName' in pggen.arg('FirstName').
	name string
	// Default value to use for the param when executing the query on Postgres.
	// Like 'joe' in pggen.arg('FirstName', 'joe').
	defaultVal string
	// The postgres type of this param as reported by Postgres.
	pgType string
	// The Go type to use in generated for this param.
	goType string
}

// cmdTag is the command tag reported by Postgres when running the TemplateQuery.
// See "command tag" in https://www.postgresql.org/docs/current/protocol-message-formats.html
type cmdTag string

const (
	tagSelect cmdTag = "select"
	tagInsert cmdTag = "insert"
	tagUpdate cmdTag = "update"
	tagDelete cmdTag = "delete"
)

// typedQuery is an enriched form of TemplateQuery after running it on Postgres to get
// information about the TemplateQuery.
type typedQuery struct {
	// Name of the query, from the comment preceding the query. Like 'FindAuthors'
	// in:
	//     -- name: FindAuthors :many
	Name string
	// The command tag that Postgres reports after running the query.
	tag cmdTag
	// The SQL query, with pggen functions replaced with Postgres syntax. Ready
	// to run with PREPARE.
	PreparedSQL string
	// The input parameters to the query.
	Inputs []param
	// The output parameters to the query.
	Outputs []param
}

// emitAll emits all query files.
func emitAll(outDir string, queries []queryFile) error {
	tmpl, err := parseQueryTemplate()
	if err != nil {
		return err
	}
	for _, query := range queries {
		if err := emitQueryFile(outDir, query, tmpl); err != nil {
			return err
		}
	}
	return nil
}

func parseQueryTemplate() (*template.Template, error) {
	statikFS, err := fs.New()
	if err != nil {
		return nil, fmt.Errorf("create statik filesystem: %w", err)
	}
	tmplFile, err := statikFS.Open("/query.gotemplate")
	if err != nil {
		return nil, fmt.Errorf("open embedded template file: %w", err)
	}
	tmplBytes, err := ioutil.ReadAll(tmplFile)
	if err != nil {
		return nil, fmt.Errorf("read embedded template file: %w", err)
	}

	tmpl, err := template.New("gen_query").Parse(string(tmplBytes))
	if err != nil {
		return nil, fmt.Errorf("parse query.gotemplate: %w", err)
	}
	return tmpl, nil
}

// emitQueryFile emits a single query file.
func emitQueryFile(outDir string, query queryFile, tmpl *template.Template) (mErr error) {
	base := filepath.Base(query.Src)
	out := filepath.Join(outDir, base+".go")
	file, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY, 0644)
	defer errs.Capture(&mErr, file.Close, "close emit query file")
	if err != nil {
		return fmt.Errorf("open generated query file for writing: %w", err)
	}
	if err := tmpl.ExecuteTemplate(file, "gen_query", query); err != nil {
		return fmt.Errorf("execute generated query file template %s: %w", out, err)
	}
	return nil
}
