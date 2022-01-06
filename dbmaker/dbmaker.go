// Package dbmaker creates SQLITE databases for various lexica, so I can use
// them in my word game empire.
package dbmaker

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/domino14/macondo/alphabet"
	"github.com/domino14/macondo/gaddag"

	// sqlite3 db driver is needed for the word db maker
	_ "github.com/mattn/go-sqlite3"
)

type Alphagram struct {
	words          []string
	combinations   uint64
	alphagram      string
	wordCount      uint8
	uniqToLexSplit uint8
	updateToLex    uint8
}

func (a *Alphagram) String() string {
	return fmt.Sprintf("Alphagram: %s (%d)", a.alphagram, a.combinations)
}

func (a *Alphagram) pointValue(dist *alphabet.LetterDistribution) uint8 {
	pts := uint8(0)
	for _, rn := range a.alphagram {
		pts += dist.PointValues[rn]
	}
	return pts
}

func (a *Alphagram) numVowels(dist *alphabet.LetterDistribution) uint8 {
	vowels := uint8(0)
	vowelMap := map[rune]bool{}
	for _, v := range dist.Vowels {
		vowelMap[v] = true
	}

	for _, rn := range a.alphagram {
		if vowelMap[rn] {
			vowels++
		}
	}
	return vowels
}

type AlphByCombos []Alphagram // used to be []*Alphagram

func (a AlphByCombos) Len() int      { return len(a) }
func (a AlphByCombos) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a AlphByCombos) Less(i, j int) bool {
	// XXX: Existing aerolith dbs don't sort by alphagram to break ties.
	// (It's sort of random unfortunately)
	// The DBs generated by this tool will be slightly off. We must continue
	// to use the old DBs until there is a lexicon update :(
	if a[i].combinations == a[j].combinations {
		return a[i].alphagram < a[j].alphagram
	}
	return a[i].combinations > a[j].combinations
}

type LexiconSymbolDefinition struct {
	In     string // The word is in this lexicon
	NotIn  string // The word is not in this lexicon
	Symbol string // The corresponding lexicon symbol
}

const CurrentVersion = 6

func exitIfError(err error) {
	if err != nil {
		log.Fatal().Err(err).Msg("")
	}
}

// create a sqlite db for this lexicon name.
func createSqliteDb(outputDir string, lexiconName string, quitIfExists bool) (
	string, error) {
	dbName := outputDir + "/" + lexiconName + ".db"

	if quitIfExists {
		_, err := os.Stat(dbName)
		if err == nil {
			return "", fmt.Errorf("db %v existed, and not overwriting it; "+
				"use -force if you would like to overwrite", dbName)
		}
	}

	os.Remove(dbName)
	sqlStmt := `
	CREATE TABLE alphagrams (probability int, alphagram varchar(20),
	    length int, combinations int, num_anagrams int,
		point_value int, num_vowels int, contains_word_uniq_to_lex_split int,
		contains_update_to_lex int, difficulty int);

	CREATE TABLE words (word varchar(20), alphagram varchar(20),
	    lexicon_symbols varchar(5), definition varchar(512),
	    front_hooks varchar(26), back_hooks varchar(26),
	    inner_front_hook int, inner_back_hook int);

	CREATE TABLE deletedwords (word varchar(20), length int);

	CREATE INDEX alpha_index on alphagrams(alphagram);
	CREATE INDEX prob_index on alphagrams(probability, length);
	CREATE INDEX word_index on words(word);
	CREATE INDEX alphagram_index on words(alphagram);
	CREATE INDEX length_index on alphagrams(length);
	CREATE INDEX difficulty_index on alphagrams(difficulty);

	CREATE INDEX num_anagrams_index on alphagrams(num_anagrams);
	CREATE INDEX point_value_index on alphagrams(point_value);
	CREATE INDEX num_vowels_index on alphagrams(num_vowels);
	CREATE INDEX uniq_word_index on alphagrams(contains_word_uniq_to_lex_split);
	CREATE INDEX update_word_index on alphagrams(contains_update_to_lex);

	CREATE TABLE db_version (version integer);
	`
	db, err := sql.Open("sqlite3", dbName)
	exitIfError(err)
	log.Info().Msgf("Opened database file at %v for writing", dbName)
	defer db.Close()

	_, err = db.Exec(sqlStmt)
	exitIfError(err)
	return dbName, nil
}

func CreateLexiconDatabase(lexiconName string, lexiconInfo *LexiconInfo, lexMap LexiconMap,
	outputDir string, quitIfExists bool) {

	log.Info().Msgf("Creating lexicon database for %v", lexiconName)

	dbName, err := createSqliteDb(outputDir, lexiconName, quitIfExists)
	if err != nil {
		log.Error().Err(err).Msg("")
		return
	}

	definitions, alphagrams := populateAlphsDefs(lexiconInfo.LexiconFilename,
		lexiconInfo.Combinations, lexiconInfo.LetterDistribution)
	log.Debug().Msg("Sorting by probability")
	alphs := alphaMapValues(alphagrams)
	sort.Sort(AlphByCombos(alphs))

	var probs [16]uint32

	alphInsertQuery := `
	INSERT INTO alphagrams(probability, alphagram, length, combinations,
		num_anagrams, point_value, num_vowels, contains_word_uniq_to_lex_split,
		contains_update_to_lex, difficulty)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	wordInsertQuery := `
	INSERT INTO words (word, alphagram, lexicon_symbols, definition,
		front_hooks, back_hooks, inner_front_hook, inner_back_hook)
	VALUES(?, ?, ?, ?, ?, ?, ?, ?)`

	db, err := sql.Open("sqlite3", dbName)
	exitIfError(err)
	tx, err := db.Begin()
	exitIfError(err)

	alphStmt, err := tx.Prepare(alphInsertQuery)
	exitIfError(err)
	wordStmt, err := tx.Prepare(wordInsertQuery)
	exitIfError(err)
	defer alphStmt.Close()
	defer wordStmt.Close()
	dawg := lexiconInfo.Dawg
	rDawg := lexiconInfo.RDawg

	lexFamily, err := lexMap.familyName(lexiconName)
	exitIfError(err)

	latestCSW := lexMap.newestInFamily(FamilyCSW)
	latestTWL := lexMap.newestInFamily(FamilyTWL)
	latestCSW.Initialize()
	latestTWL.Initialize()

	priorLex, err := lexMap.priorLexicon(lexFamily, lexiconName)
	if err != nil {
		// ignore this
		log.Err(err).Msg("no prior lexicon, ignoring...")
	}

	for idx, alph := range alphs {
		if idx%10000 == 0 {
			log.Debug().Msgf("%d...", idx)
		}
		wl := len([]rune(alph.alphagram))
		if wl <= 15 {
			probs[wl]++
		}
		lexSymbolsList := []string{}
		for _, word := range alph.words {
			backHooks := sortedHooks(gaddag.FindHooks(dawg, word, gaddag.BackHooks),
				lexiconInfo.LetterDistribution)
			frontHooks := sortedHooks(gaddag.FindHooks(rDawg, word, gaddag.FrontHooks),
				lexiconInfo.LetterDistribution)
			frontInnerHook := 0
			backInnerHook := 0
			if gaddag.FindInnerHook(dawg, word, gaddag.BackInnerHook) {
				backInnerHook = 1
			}
			if gaddag.FindInnerHook(dawg, word, gaddag.FrontInnerHook) {
				frontInnerHook = 1
			}

			def := definitions[word]
			alphagram := alph.alphagram
			theseLexSymbols := findLexSymbols(word, latestCSW, latestTWL, lexFamily, priorLex)
			wordStmt.Exec(word, alphagram, theseLexSymbols, def,
				frontHooks, backHooks, frontInnerHook, backInnerHook)
			lexSymbolsList = append(lexSymbolsList, theseLexSymbols)
		}

		_, err = alphStmt.Exec(probs[wl], alph.alphagram, wl, alph.combinations,
			len(alph.words), alph.pointValue(lexiconInfo.LetterDistribution),
			alph.numVowels(lexiconInfo.LetterDistribution),
			containsWordUniqueToLexSplit(lexSymbolsList),
			containsUpdateToLex(lexSymbolsList),
			alphagramDifficulty(alph.alphagram, lexiconInfo.Difficulties))
		exitIfError(err)

	}
	tx.Commit()

	deletedWords := []string{}
	// Check for deletions.
	if priorLex != nil {
		priorLex.Initialize()
		definitions, _ := populateAlphsDefs(priorLex.LexiconFilename,
			priorLex.Combinations, priorLex.LetterDistribution)
		for word := range definitions {
			if !gaddag.FindWord(lexiconInfo.Dawg, word) {
				deletedWords = append(deletedWords, word)
			}
		}
	}

	deletedWordInsertQuery := `
	INSERT INTO deletedwords (word, length)
	VALUES(?, ?)`

	if len(deletedWords) > 0 {
		sort.Strings(deletedWords)
		tx, err := db.Begin()
		exitIfError(err)

		wordStmt, err := tx.Prepare(deletedWordInsertQuery)
		exitIfError(err)
		defer wordStmt.Close()

		for _, word := range deletedWords {
			_, err = wordStmt.Exec(word, len(word))
			exitIfError(err)
		}
		tx.Commit()
	}

	_, err = db.Exec("INSERT INTO db_version(version) VALUES(?)", CurrentVersion)
	exitIfError(err)
	// log the word length dict to screen. This is needed for the lexica.yaml
	// fixture in webolith.
	logWordLengths(probs)
}

func logWordLengths(lengths [16]uint32) {
	mp := map[string]uint32{}
	for idx, lgt := range lengths {
		if lgt == 0 {
			continue
		}
		mp[strconv.Itoa(idx)] = lgt
	}
	bts, err := json.Marshal(mp)
	if err != nil {
		panic(err.Error())
	}
	log.Info().Msgf("Word lengths: '%s'", string(bts))
}

func FixDefinitions(lexiconName string, lexMap LexiconMap) {
	_, err := os.Stat(lexiconName + ".db")
	if os.IsNotExist(err) {
		log.Fatal().Msg("Database does not exist in this directory.")
	}
	db, err := sql.Open("sqlite3", lexiconName+".db")
	exitIfError(err)

	lexiconInfo, err := lexMap.GetLexiconInfo(lexiconName)
	exitIfError(err)
	lexiconInfo.Initialize()

	definitions, _ := populateAlphsDefs(lexiconInfo.LexiconFilename,
		lexiconInfo.Combinations, lexiconInfo.LetterDistribution)

	definitionEditQuery := `
	UPDATE words SET definition = ? WHERE word = ?
	`

	tx, err := db.Begin()
	exitIfError(err)

	defStmt, err := tx.Prepare(definitionEditQuery)
	exitIfError(err)

	for word, def := range definitions {
		_, err := defStmt.Exec(def, word)
		exitIfError(err)
	}

	tx.Commit()

	defStmt.Close()
	db.Close()

}

func FixLexiconSymbols(lexiconName string, lexMap LexiconMap) {

	_, err := os.Stat(lexiconName + ".db")
	if os.IsNotExist(err) {
		log.Fatal().Msg("Database does not exist in this directory.")
	}
	db, err := sql.Open("sqlite3", lexiconName+".db")
	exitIfError(err)

	tx, err := db.Begin()
	exitIfError(err)

	lexiconInfo, err := lexMap.GetLexiconInfo(lexiconName)
	exitIfError(err)
	lexiconInfo.Initialize()

	_, alphagrams := populateAlphsDefs(lexiconInfo.LexiconFilename,
		lexiconInfo.Combinations, lexiconInfo.LetterDistribution)

	lexSymbolEditQuery := `
	UPDATE words SET lexicon_symbols = ? WHERE word = ?
	`

	alphaLexEditQuery := `
	UPDATE alphagrams SET contains_word_uniq_to_lex_split = ?,
		contains_update_to_lex = ?
	WHERE alphagram = ?`

	alphStmt, err := tx.Prepare(alphaLexEditQuery)
	exitIfError(err)

	wordStmt, err := tx.Prepare(lexSymbolEditQuery)
	exitIfError(err)

	defer alphStmt.Close()
	defer wordStmt.Close()

	lexFamily, err := lexMap.familyName(lexiconName)
	exitIfError(err)

	latestCSW := lexMap.newestInFamily(FamilyCSW)
	latestTWL := lexMap.newestInFamily(FamilyTWL)

	priorLex, err := lexMap.priorLexicon(lexFamily, lexiconName)
	if err != nil {
		// ignore this
		log.Err(err).Msg("no prior lexicon, ignoring...")
	}
	for _, alphagramObj := range alphagrams {
		lexSymbolsList := []string{}
		for _, word := range alphagramObj.words {
			theseLexSymbols := findLexSymbols(word, latestCSW, latestTWL, lexFamily, priorLex)
			_, err := wordStmt.Exec(theseLexSymbols, word)
			if err != nil {
				log.Fatal().Err(err).Msg("")
			}
			lexSymbolsList = append(lexSymbolsList, theseLexSymbols)
		}
		_, err := alphStmt.Exec(containsWordUniqueToLexSplit(lexSymbolsList),
			containsUpdateToLex(lexSymbolsList), alphagramObj.alphagram)
		if err != nil {
			log.Fatal().Err(err).Msg("")
		}
	}
	tx.Commit()

}

// MigrateLexiconDatabase assumes the database has already been created with
// a previous version of this program. At the minimum, the schema looks like:
// sqlStmt := `
// CREATE TABLE alphagrams (probability int, alphagram varchar(20),
//     length int, combinations int, num_anagrams int);
//
// CREATE TABLE words (word varchar(20), alphagram varchar(20),
//     lexicon_symbols varchar(5), definition varchar(512),
//     front_hooks varchar(26), back_hooks varchar(26),
//     inner_front_hook int, inner_back_hook int);
//
// CREATE INDEX alpha_index on alphagrams(alphagram);
// CREATE INDEX prob_index on alphagrams(probability, length);
// CREATE INDEX word_index on words(word);
// CREATE INDEX alphagram_index on words(alphagram);
// `
// This function assumes the above schema.
func MigrateLexiconDatabase(lexiconName string, lexiconInfo *LexiconInfo) {
	dbName := "./" + lexiconName + ".db"

	db, err := sql.Open("sqlite3", dbName)
	exitIfError(err)
	var version int
	err = db.QueryRow("SELECT version FROM db_version").Scan(&version)
	switch {
	case err == sql.ErrNoRows:
		log.Fatal().Msg("There is a version table but it has no values in it")
	case err != nil:
		if err.Error() == "no such table: db_version" {
			log.Info().Msg("No version table, creating one...")
			_, err = db.Exec("CREATE TABLE db_version (version integer)")
			if err != nil {
				log.Fatal().Err(err).Msg("")
			}
			_, err = db.Exec("INSERT INTO db_version(version) VALUES(?)", 1)
			if err != nil {
				log.Fatal().Err(err).Msg("")
			}
			version = 1
		} else {
			log.Fatal().Err(err).Msg("")
		}
	default:
		if version == CurrentVersion {
			log.Info().Msgf("DB Version is up to date (version %d)", version)
		} else {
			log.Info().Msgf("Version of this table is %d, moving to %d", version,
				version+1)
		}
	}

	if version == 1 {
		log.Info().Msg("Migrating to version 2...")
		migrateToV2(db, lexiconInfo.LetterDistribution)
		log.Info().Msg("Run again to migrate to version 3")
	}
	if version == 2 {
		log.Info().Msg("Migrating to version 3...")
		migrateToV3(db)
		log.Info().Msg("Run again to migrate to version 4")
	}
	if version == 3 {
		log.Info().Msg("Migrating to version 4...")
		migrateToV4(db)
		log.Info().Msg("Run again to migrate to version 5")
	}
	if version == 4 {
		log.Info().Msg("Migrating to version 5...")
		migrateToV5(db, lexiconInfo)
	}
	if version == 5 {
		log.Info().Msg("Migrating to version 6...")
		migrateToV6(db)
	}

}

func migrateToV2(db *sql.DB, dist *alphabet.LetterDistribution) {
	// Version 2 has the following improvements:
	// An index on point value, and point value
	// An index on num anagrams, and num anagrams
	// An index on num vowels, and num vowels

	_, err := db.Exec(`
			ALTER TABLE alphagrams ADD COLUMN num_anagrams int;
			ALTER TABLE alphagrams ADD COLUMN point_value int;
			ALTER TABLE alphagrams ADD COLUMN num_vowels int;

			CREATE INDEX num_anagrams_index on alphagrams(num_anagrams);
			CREATE INDEX point_value_index on alphagrams(point_value);
			CREATE INDEX num_vowels_index on alphagrams(num_vowels);
			`)
	exitIfError(err)

	// Read in all the alphagrams.
	rows, err := db.Query(`
			SELECT words.alphagram, count() AS word_ct FROM words
			INNER JOIN alphagrams on words.alphagram = alphagrams.alphagram
			GROUP BY words.alphagram
			`)
	exitIfError(err)
	defer rows.Close()

	tx, err := db.Begin()
	exitIfError(err)

	updateQuery := `
		UPDATE alphagrams SET num_anagrams = ?, point_value = ?, num_vowels = ?
		WHERE alphagram = ?
	`

	alphagrams := []Alphagram{}
	// Read all the rows and update alphagrams.
	for rows.Next() {
		var (
			alph      string
			wordCount int
		)
		if err := rows.Scan(&alph, &wordCount); err != nil {
			log.Fatal().Err(err).Msg("")
		}
		alphagrams = append(alphagrams, Alphagram{alphagram: alph,
			wordCount: uint8(wordCount)})
	}

	i := 0
	updateStmt, err := tx.Prepare(updateQuery)
	exitIfError(err)
	for _, alph := range alphagrams {
		_, err := updateStmt.Exec(alph.wordCount, alph.pointValue(dist),
			alph.numVowels(dist), alph.alphagram)
		if err != nil {
			log.Fatal().Err(err).Msg("")
		}
		i++
		if i%10000 == 0 {
			log.Debug().Msgf("%d...", i)
		}
	}
	tx.Commit()

	_, err = db.Exec("UPDATE db_version SET version = ?", 2)
	exitIfError(err)
}

func migrateToV3(db *sql.DB) {
	_, err := db.Exec("CREATE INDEX length_index on alphagrams(length);")
	exitIfError(err)
	_, err = db.Exec("UPDATE db_version SET version = ?", 3)
	exitIfError(err)
}

func migrateToV4(db *sql.DB) {
	_, err := db.Exec(`
	ALTER TABLE alphagrams ADD COLUMN contains_word_uniq_to_lex_split int;
	ALTER TABLE alphagrams ADD COLUMN contains_update_to_lex int;

	CREATE INDEX uniq_word_index on alphagrams(contains_word_uniq_to_lex_split);
	CREATE INDEX update_word_index on alphagrams(contains_update_to_lex);
	`)
	exitIfError(err)
	log.Info().Msg("Created new columns and indices")
	// Read in all the words.
	rows, err := db.Query(`
	SELECT word, alphagram, lexicon_symbols from words
	order by alphagram
	`)
	exitIfError(err)
	defer rows.Close()

	tx, err := db.Begin()
	exitIfError(err)

	updateQuery := `
	UPDATE alphagrams SET contains_word_uniq_to_lex_split = ?,
		contains_update_to_lex = ?
	WHERE alphagram = ?
	`

	alphagrams := []Alphagram{}
	lastAlph := ""
	lastLexSymbolsList := []string{}

	for rows.Next() {
		var (
			word           string
			alph           string
			lexiconSymbols string
		)
		if err := rows.Scan(&word, &alph, &lexiconSymbols); err != nil {
			log.Fatal().Err(err).Msg("")
		}
		//log.Println(word, alph, lexiconSymbols)

		if alph != lastAlph && lastAlph != "" {
			// We have a new alphagram.
			uniqToLexSplit := containsWordUniqueToLexSplit(lastLexSymbolsList)
			updateToLex := containsUpdateToLex(lastLexSymbolsList)
			alphagrams = append(alphagrams, Alphagram{alphagram: lastAlph,
				uniqToLexSplit: uniqToLexSplit, updateToLex: updateToLex})

			lastLexSymbolsList = []string{}
		}

		lastAlph = alph
		lastLexSymbolsList = append(lastLexSymbolsList, lexiconSymbols)
	}

	// Update the very last one too.
	alphagrams = append(alphagrams, Alphagram{alphagram: lastAlph,
		uniqToLexSplit: containsWordUniqueToLexSplit(lastLexSymbolsList),
		updateToLex:    containsUpdateToLex(lastLexSymbolsList)})

	i := 0
	updateStmt, err := tx.Prepare(updateQuery)
	exitIfError(err)

	for _, alph := range alphagrams {
		_, err := updateStmt.Exec(alph.uniqToLexSplit, alph.updateToLex,
			alph.alphagram)
		if err != nil {
			log.Fatal().Err(err).Msg("")
		}
		i++
		if i%10000 == 0 {
			log.Printf("%d...", i)
		}
	}
	tx.Commit()

	_, err = db.Exec("UPDATE db_version SET version = ?", 4)
	exitIfError(err)
}

func migrateToV5(db *sql.DB, lexiconInfo *LexiconInfo) {
	_, err := db.Exec(`
	-- ALTER TABLE alphagrams ADD COLUMN playability int;
	ALTER TABLE alphagrams ADD COLUMN difficulty int;

	-- CREATE INDEX playability_index on alphagrams(playability);
	CREATE INDEX difficulty_index on alphagrams(difficulty);
	`)
	exitIfError(err)
	log.Info().Msg("Created new columns and indices")

	loadDifficulty(db, lexiconInfo)

	_, err = db.Exec("UPDATE db_version SET version = ?", 5)
	exitIfError(err)
}

func migrateToV6(db *sql.DB) {
	_, err := db.Exec(`
		CREATE TABLE deletedwords (word varchar(20), length int);
	`)
	if err != nil {
		log.Fatal().Err(err).Msg("")
	}
	log.Info().Msg("Created new deletedwords table")

	_, err = db.Exec("UPDATE db_version SET version = ?", 6)
	exitIfError(err)
}

func sortedHooks(hooks []rune, dist *alphabet.LetterDistribution) string {
	w := alphabet.Word{Word: string(hooks), Dist: dist}
	return w.MakeAlphagram()
}

func findLexSymbols(word string, latestCSW, latestTWL *LexiconInfo, lexFamily FamilyName,
	priorLex *LexiconInfo) string {

	symbols := ""

	if priorLex != nil {
		if !gaddag.FindWord(priorLex.Dawg, word) && !strings.Contains(symbols, LexiconUpdateSymbol) {
			symbols += LexiconUpdateSymbol
		}
	}
	if lexFamily == FamilyCSW && !gaddag.FindWord(latestTWL.Dawg, word) &&
		!strings.Contains(symbols, CSWOnlySymbol) {
		symbols += CSWOnlySymbol
	}
	if lexFamily == FamilyTWL && !gaddag.FindWord(latestCSW.Dawg, word) &&
		!strings.Contains(symbols, TWLOnlySymbol) {
		symbols += TWLOnlySymbol
	}

	return symbols
}

// This is a bit of a special function, used only for the annoying lexical
// split in English-language Scrabble. If the lexiconName is "America",
// or "NWL18", this will return a 1 if any of the strings in the lexSymbols
// array contains a $ sign. If the lexiconName is "CSW19", the string to
// look for is #.
// All other cases return a 0.
// Note that this will need to be updated when new versions of America/ CSW
// are added.
func containsWordUniqueToLexSplit(lexSymbolsList []string) uint8 {

	for _, symbols := range lexSymbolsList {
		if strings.Contains(symbols, "$") || strings.Contains(symbols, "#") {
			return 1
		}
	}
	return 0
}

// All updates to a lexicon will be indicated with a + symbol basically.
func containsUpdateToLex(lexSymbolsList []string) uint8 {

	for _, symbols := range lexSymbolsList {
		if strings.Contains(symbols, "+") {
			return 1
		}
	}
	return 0
}

// The values of the map.
func alphaMapValues(theMap map[string]Alphagram) []Alphagram {
	x := make([]Alphagram, len(theMap)) // thelf
	i := 0
	for _, value := range theMap {
		x[i] = value
		i++
	}
	return x
}

func populateAlphsDefs(filename string, combinations func(string, bool) uint64,
	dist *alphabet.LetterDistribution) (map[string]string, map[string]Alphagram) {

	definitions := make(map[string]*FullDefinition)
	alphagrams := make(map[string]Alphagram)
	file, _ := os.Open(filename)
	// XXX: Check error
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) > 0 {
			word := alphabet.Word{Word: strings.ToUpper(fields[0]), Dist: dist}
			definition := ""
			if len(fields) > 1 {
				definition = strings.Join(fields[1:], " ")
			}
			addToDefinitions(word.Word, definition, definitions)
			alphagram := word.MakeAlphagram()
			alph, ok := alphagrams[alphagram]
			if !ok {
				alphagrams[alphagram] = Alphagram{
					[]string{word.Word},
					combinations(alphagram, true),
					alphagram, 0, 0, 0}
			} else {
				alph.words = append(alph.words, word.Word)
				alphagrams[alphagram] = alph
			}
		}
	}
	file.Close()

	definitionMap := expandDefinitions(definitions)

	return definitionMap, alphagrams
}
