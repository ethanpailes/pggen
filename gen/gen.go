package gen

import (
	"database/sql"
	"fmt"
	"io/ioutil"
	"os"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	_ "github.com/lib/pq"
)

// `pggen.Config` contains a collection of configuration options for the
// the codegenerator
type Config struct {
	// The path to a configuration file in TOML format containing information
	// about the database objects that pggen should generate code for.
	ConfigFilePath string
	// The name of the file to which the output should be written.
	OutputFileName string
	// A list of postgres connection strings to be used to connect to the
	// database. They tried in order until one is found where `DB.Ping` works.
	ConnectionStrings []string
	// A list of var patterns which disable pggen when they match the environment.
	DisableVars []string
	// A list of var patterns which must match against the environment in order for
	// pggen to run.
	EnableVars []string
	// The verbosity level of the code generator. -1 means quiet mode,
	// 0 (the default) means normal mode, and 1 means verbose mode.
	Verbosity int
}

// An instantiation of a pggen codegenerator
type Generator struct {
	// The user supplied configuration for this run of the pggen
	// codegenerator.
	config Config
	// The name of the package that all generated code is a part of.
	// Inferred from `config.OutputFileName`.
	pkg string
	// The database connection we use to gather information required
	// for code generation.
	db *sql.DB
	// A table mapping go type name for a table struct to the postgres
	// name for that table.
	tableTyNameToTableName map[string]string
	// Metadata about the tables to be generated, maps from the
	// names of the tables in postgres to info about them.
	tables map[string]*tableGenInfo
	// The packages which need to be imported into the emitted
	// file.
	imports map[string]bool
	// The clearing house for types that we emit. They all go here
	// before being generated for real. We do this to prevent generating
	// the same type twice.
	types typeSet
	// A table mapping postgres primitive types to go types. Produced
	// by taking a default table an applying the user-provided type
	// overrides to it.
	pgType2GoType map[string]*goTypeInfo
	// This generator should do nothing because a disable var matched
	disabledByDisableVar bool
	// This generated should do nothing becase an enable var failed to match
	disabledByEnableVar bool
}

// Print `output` at a normal verbosity level
func (g *Generator) info(output string) {
	if g.config.Verbosity >= 0 {
		fmt.Print(output)
	}
}

// Print `output` at a normal verbosity level, formatting the output
// using the standard formatting codes from `fmt`.
func (g *Generator) infof(format string, a ...interface{}) {
	g.info(fmt.Sprintf(format, a...))
}

func (g *Generator) warn(output string) {
	if g.config.Verbosity >= -1 {
		fmt.Fprint(os.Stderr, output)
	}
}

func (g *Generator) warnf(format string, a ...interface{}) {
	g.warn(fmt.Sprintf("WARN: "+format, a...))
}

func FromConfig(config Config) (*Generator, error) {
	if anyVarPatternMatches(config.DisableVars) {
		return &Generator{disabledByDisableVar: true}, nil
	}
	if !allVarPatternsMatch(config.EnableVars) {
		return &Generator{disabledByEnableVar: true}, nil
	}

	var err error
	var db *sql.DB
	for _, connStr := range config.ConnectionStrings {
		if len(connStr) > 0 && connStr[0] == '$' {
			connStr = os.Getenv(connStr[1:])
		}

		db, err = sql.Open("postgres", connStr)
		if err != nil {
			db = nil
			continue
		}

		err = db.Ping()
		if err == nil {
			break
		} else {
			db = nil
			continue
		}
	}
	if db == nil {
		return nil, fmt.Errorf(
			"unable to connect with any of the provided connection strings",
		)
	}

	pkg, err := dirOf(config.OutputFileName)
	if err != nil {
		return nil, err
	}

	return &Generator{
		config:  config,
		db:      db,
		pkg:     pkg,
		imports: map[string]bool{},
		types:   newTypeSet(),
	}, nil
}

// Generate the code that this generator has been configured for
func (g *Generator) Gen() error {
	if g.disabledByDisableVar {
		g.info("pggen: doing nothing because a disable var matched\n")
		return nil
	}

	if g.disabledByEnableVar {
		g.info("pggen: doing nothing because an enable var failed to match\n")
		return nil
	}

	conf, err := g.setupGenEnv()
	if err != nil {
		return err
	}

	err = g.genPrelude()
	if err != nil {
		return err
	}

	//
	// Generate the code based on database objects
	//

	var body strings.Builder

	err = g.genPGClient(&body, conf.Tables)
	if err != nil {
		return err
	}

	// Tables must be generated first to ensure that the type for a table is generated
	// by genTables rather than synthesized from a query result.
	err = g.genTables(&body, conf.Tables)
	if err != nil {
		return err
	}

	err = g.genStoredFuncs(&body, conf.StoredFunctions)
	if err != nil {
		return err
	}

	err = g.genQueries(&body, conf.Queries)
	if err != nil {
		return err
	}

	err = g.genStmts(&body, conf.Stmts)
	if err != nil {
		return err
	}

	//
	// Write the generated code to the file
	//

	var out strings.Builder

	// generate imports
	_, err = out.WriteString("// Code generated by pggen DO NOT EDIT\n")
	if err != nil {
		return err
	}
	_, err = out.WriteString(fmt.Sprintf(`
package %s

import (
`, g.pkg))
	if err != nil {
		return err
	}
	sortedPkgs := make([]string, len(g.imports))[:0]
	for pkg := range g.imports {
		if len(pkg) > 0 {
			sortedPkgs = append(sortedPkgs, pkg)
		}
	}
	sort.Strings(sortedPkgs)
	for _, pkg := range sortedPkgs {
		_, err = out.WriteString(fmt.Sprintf("	%s\n", pkg))
		if err != nil {
			return err
		}
	}
	_, err = out.WriteString(")\n\n")
	if err != nil {
		return err
	}

	_, err = out.WriteString(body.String())
	if err != nil {
		return err
	}

	err = g.types.gen(&out)
	if err != nil {
		return err
	}

	return writeGoFile(g.config.OutputFileName, []byte(out.String()))
}

func (g *Generator) setupGenEnv() (*dbConfig, error) {
	g.infof("pggen: using config '%s'\n", g.config.ConfigFilePath)
	confData, err := ioutil.ReadFile(g.config.ConfigFilePath)
	if err != nil {
		return nil, err
	}

	// parse the config file
	var conf dbConfig
	tomlMd, err := toml.Decode(string(confData), &conf)
	if err != nil {
		return nil, fmt.Errorf("while parsing config file: %s", err.Error())
	}
	for _, unknownKey := range tomlMd.Undecoded() {
		fmt.Fprintf(
			os.Stderr,
			"WARN: unknown config file key: '%s'\n",
			unknownKey.String(),
		)
	}
	err = conf.normalize()
	if err != nil {
		return nil, err
	}

	// Apply the type overrides
	err = g.initTypeTable(conf.TypeOverrides)
	if err != nil {
		return nil, err
	}

	// Place metadata about all tables in a hashtable to later
	// access by the table and query generation phases.
	err = g.populateTableInfo(conf.Tables)
	if err != nil {
		return nil, err
	}

	return &conf, nil
}
