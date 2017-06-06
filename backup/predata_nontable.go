package backup

/*
 * This file contains structs and functions related to dumping non-table-related
 * metadata on the master that needs to be restored before data is restored, such
 * as sequences and check constraints.
 */

import (
	"fmt"
	"gpbackup/utils"
	"io"
	"sort"
	"strings"
)

type SequenceDefinition struct {
	utils.Relation
	QuerySequenceDefinition
}

/*
 * Functions to print to the predata file
 */

/*
 * This function calls per-table functions to get constraints related to each
 * table, then consolidates them in two slices holding all constraints for all
 * tables.  Two slices are needed because FOREIGN KEY constraints must be dumped
 * after PRIMARY KEY constraints, so they're separated out to be handled last.
 */
func ConstructConstraintsForAllTables(connection *utils.DBConn, tables []utils.Relation) ([]string, []string) {
	allConstraints := make([]string, 0)
	allFkConstraints := make([]string, 0)
	for _, table := range tables {
		constraintList := GetConstraints(connection, table.RelationOid)
		tableConstraints, tableFkConstraints := ProcessConstraints(table, constraintList)
		allConstraints = append(allConstraints, tableConstraints...)
		allFkConstraints = append(allFkConstraints, tableFkConstraints...)
	}
	return allConstraints, allFkConstraints
}

/*
 * There's no built-in function to generate constraint definitions like there is for other types of
 * metadata, so this function constructs them.
 */
func ProcessConstraints(table utils.Relation, constraints []QueryConstraint) ([]string, []string) {
	alterStr := fmt.Sprintf("\n\nALTER TABLE ONLY %s ADD CONSTRAINT %s %s;", table.ToString(), "%s", "%s")
	commentStr := fmt.Sprintf("\n\nCOMMENT ON CONSTRAINT %s ON %s IS '%s';", "%s", table.ToString(), "%s")
	cons := make([]string, 0)
	fkCons := make([]string, 0)
	for _, constraint := range constraints {
		conStr := fmt.Sprintf(alterStr, constraint.ConName, constraint.ConDef)
		if constraint.ConComment != "" {
			conStr += fmt.Sprintf(commentStr, constraint.ConName, constraint.ConComment)
		}
		if constraint.ConType == "f" {
			fkCons = append(fkCons, conStr)
		} else {
			cons = append(cons, conStr)
		}
	}
	return cons, fkCons
}

func PrintConstraintStatements(predataFile io.Writer, constraints []string, fkConstraints []string) {
	sort.Strings(constraints)
	sort.Strings(fkConstraints)
	for _, constraint := range constraints {
		fmt.Fprintln(predataFile, constraint)
	}
	for _, constraint := range fkConstraints {
		fmt.Fprintln(predataFile, constraint)
	}
}

func PrintCreateSchemaStatements(predataFile io.Writer, schemas []utils.Schema) {
	for _, schema := range schemas {
		fmt.Fprintln(predataFile)
		if schema.SchemaName != "public" {
			fmt.Fprintf(predataFile, "\nCREATE SCHEMA %s;", schema.ToString())
		}
		if schema.Owner != "" {
			fmt.Fprintf(predataFile, "\nALTER SCHEMA %s OWNER TO %s;", schema.ToString(), utils.QuoteIdent(schema.Owner))
		}
		if schema.Comment != "" {
			fmt.Fprintf(predataFile, "\nCOMMENT ON SCHEMA %s IS '%s';", schema.ToString(), schema.Comment)
		}
	}
}

func GetAllSequenceDefinitions(connection *utils.DBConn) []SequenceDefinition {
	allSequences := GetAllSequences(connection)
	sequenceDefs := make([]SequenceDefinition, 0)
	for _, seq := range allSequences {
		sequence := GetSequenceDefinition(connection, seq.ToString())
		sequenceDef := SequenceDefinition{seq, sequence}
		sequenceDefs = append(sequenceDefs, sequenceDef)
	}
	return sequenceDefs
}

/*
 * This function is largely derived from the dumpSequence() function in pg_dump.c.  The values of
 * minVal and maxVal come from SEQ_MINVALUE and SEQ_MAXVALUE, defined in include/commands/sequence.h.
 */
func PrintCreateSequenceStatements(predataFile io.Writer, sequences []SequenceDefinition) {
	maxVal := int64(9223372036854775807)
	minVal := int64(-9223372036854775807)
	for _, sequence := range sequences {
		fmt.Fprintln(predataFile, "\n\nCREATE SEQUENCE", sequence.ToString())
		if !sequence.IsCalled {
			fmt.Fprintln(predataFile, "\tSTART WITH", sequence.LastVal)
		}
		fmt.Fprintln(predataFile, "\tINCREMENT BY", sequence.Increment)

		if !((sequence.MaxVal == maxVal && sequence.Increment > 0) || (sequence.MaxVal == -1 && sequence.Increment < 0)) {
			fmt.Fprintln(predataFile, "\tMAXVALUE", sequence.MaxVal)
		} else {
			fmt.Fprintln(predataFile, "\tNO MAXVALUE")
		}
		if !((sequence.MinVal == minVal && sequence.Increment < 0) || (sequence.MinVal == 1 && sequence.Increment > 0)) {
			fmt.Fprintln(predataFile, "\tMINVALUE", sequence.MinVal)
		} else {
			fmt.Fprintln(predataFile, "\tNO MINVALUE")
		}
		cycleStr := ""
		if sequence.IsCycled {
			cycleStr = "\n\tCYCLE"
		}
		fmt.Fprintf(predataFile, "\tCACHE %d%s;", sequence.CacheVal, cycleStr)

		fmt.Fprintf(predataFile, "\n\nSELECT pg_catalog.setval('%s', %d, %v);\n", sequence.ToString(), sequence.LastVal, sequence.IsCalled)

		if sequence.Owner != "" {
			fmt.Fprintf(predataFile, "\n\nALTER TABLE %s OWNER TO %s;\n", sequence.ToString(), utils.QuoteIdent(sequence.Owner))
		}

		if sequence.Comment != "" {
			fmt.Fprintf(predataFile, "\n\nCOMMENT ON SEQUENCE %s IS '%s';\n", sequence.ToString(), sequence.Comment)
		}
	}
}

func PrintCreateLanguageStatements(predataFile io.Writer, procLangs []QueryProceduralLanguage, funcInfoMap map[uint32]FunctionInfo) {
	for _, procLang := range procLangs {
		quotedOwner := utils.QuoteIdent(procLang.Owner)
		quotedLanguage := utils.QuoteIdent(procLang.Name)
		fmt.Fprintf(predataFile, "\n\nCREATE ")
		if procLang.PlTrusted {
			fmt.Fprintf(predataFile, "TRUSTED ")
		}
		fmt.Fprintf(predataFile, "PROCEDURAL LANGUAGE %s;", quotedLanguage)
		/*
		 * If the handler, validator, and inline functions are in pg_pltemplate, we can
		 * dump a CREATE LANGUAGE command without specifying them individually.
		 *
		 * The schema of the handler function should match the schema of the language itself, but
		 * the inline and validator functions can be in a different schema and must be schema-qualified.
		 */

		if procLang.Handler != 0 {
			handlerInfo := funcInfoMap[procLang.Handler]
			fmt.Fprintf(predataFile, "\nALTER FUNCTION %s(%s) OWNER TO %s;", handlerInfo.QualifiedName, handlerInfo.Arguments, quotedOwner)
		}
		if procLang.Inline != 0 {
			inlineInfo := funcInfoMap[procLang.Inline]
			fmt.Fprintf(predataFile, "\nALTER FUNCTION %s(%s) OWNER TO %s;", inlineInfo.QualifiedName, inlineInfo.Arguments, quotedOwner)
		}
		if procLang.Validator != 0 {
			validatorInfo := funcInfoMap[procLang.Validator]
			fmt.Fprintf(predataFile, "\nALTER FUNCTION %s(%s) OWNER TO %s;", validatorInfo.QualifiedName, validatorInfo.Arguments, quotedOwner)
		}
		if procLang.Owner != "" {
			fmt.Fprintf(predataFile, "\nALTER LANGUAGE %s OWNER TO %s;", quotedLanguage, quotedOwner)
		}
		if procLang.Comment != "" {
			fmt.Fprintf(predataFile, "\n\nCOMMENT ON LANGUAGE %s IS '%s';", quotedLanguage, procLang.Comment)
		}
		fmt.Fprintln(predataFile)
	}
}

func PrintCreateFunctionStatements(predataFile io.Writer, funcDefs []QueryFunctionDefinition) {
	for _, funcDef := range funcDefs {
		funcFQN := utils.MakeFQN(funcDef.SchemaName, funcDef.FunctionName)
		fmt.Fprintf(predataFile, "\n\nCREATE FUNCTION %s(%s) RETURNS ", funcFQN, funcDef.Arguments)
		if funcDef.ReturnsSet && !strings.HasPrefix(funcDef.ResultType, "TABLE") {
			fmt.Fprintf(predataFile, "SETOF ")
		}
		fmt.Fprintf(predataFile, "%s AS", funcDef.ResultType)
		PrintFunctionBodyOrPath(predataFile, funcDef)
		fmt.Fprintf(predataFile, "LANGUAGE %s", funcDef.Language)
		PrintFunctionModifiers(predataFile, funcDef)
		fmt.Fprintln(predataFile, ";")

		if funcDef.Owner != "" {
			fmt.Fprintf(predataFile, "\nALTER FUNCTION %s(%s) OWNER TO %s;\n", funcFQN, funcDef.IdentArgs, utils.QuoteIdent(funcDef.Owner))
		}
		if funcDef.Comment != "" {
			fmt.Fprintf(predataFile, "\nCOMMENT ON FUNCTION %s(%s) IS '%s';\n", funcFQN, funcDef.IdentArgs, funcDef.Comment)
		}
	}
}

/*
 * This function either prints a path to an executable function (for C and
 * internal functions) or a function definition (for functions in other languages).
 */
func PrintFunctionBodyOrPath(predataFile io.Writer, funcDef QueryFunctionDefinition) {
	/*
	 * pg_proc.probin uses either NULL (in this case an empty string) or "-"
	 * to signify an unused path, for historical reasons.  See dumpFunc in
	 * pg_dump.c for details.
	 */
	if funcDef.BinaryPath != "" && funcDef.BinaryPath != "-" {
		fmt.Fprintf(predataFile, "\n'%s', '%s'\n", funcDef.BinaryPath, funcDef.FunctionBody)
	} else {
		fmt.Fprintf(predataFile, "\n%s\n", utils.DollarQuoteString(funcDef.FunctionBody))
	}
}

func PrintFunctionModifiers(predataFile io.Writer, funcDef QueryFunctionDefinition) {
	switch funcDef.SqlUsage {
	case "c":
		fmt.Fprint(predataFile, " CONTAINS SQL")
	case "m":
		fmt.Fprint(predataFile, " MODIFIES SQL DATA")
	case "n":
		fmt.Fprint(predataFile, " NO SQL")
	case "r":
		fmt.Fprint(predataFile, " READS SQL DATA")
	}
	switch funcDef.Volatility {
	case "i":
		fmt.Fprintf(predataFile, " IMMUTABLE")
	case "s":
		fmt.Fprintf(predataFile, " STABLE")
	case "v": // Default case, don't print anything else
	}
	if funcDef.IsStrict {
		fmt.Fprintf(predataFile, " STRICT")
	}
	if funcDef.IsSecurityDefiner {
		fmt.Fprintf(predataFile, " SECURITY DEFINER")
	}
	// Default cost is 1 for C and internal functions or 100 for functions in other languages
	isInternalOrC := funcDef.Language == "c" || funcDef.Language == "internal"
	if !((!isInternalOrC && funcDef.Cost == 100) || (isInternalOrC && funcDef.Cost == 1)) {
		fmt.Fprintf(predataFile, "\nCOST %v", funcDef.Cost)
	}
	if funcDef.ReturnsSet && funcDef.NumRows != 0 && funcDef.NumRows != 1000 {
		fmt.Fprintf(predataFile, "\nROWS %v", funcDef.NumRows)
	}
	if funcDef.Config != "" {
		fmt.Fprintf(predataFile, "\n%s", funcDef.Config)
	}
}

func PrintCreateAggregateStatements(predataFile io.Writer, aggDefs []QueryAggregateDefinition, funcInfoMap map[uint32]FunctionInfo) {
	for _, aggDef := range aggDefs {
		aggFQN := utils.MakeFQN(aggDef.SchemaName, aggDef.AggregateName)
		orderedStr := ""
		if aggDef.IsOrdered {
			orderedStr = "ORDERED "
		}
		fmt.Fprintf(predataFile, "\n\nCREATE %sAGGREGATE %s(%s) (\n", orderedStr, aggFQN, aggDef.Arguments)
		fmt.Fprintf(predataFile, "\tSFUNC = %s,\n", funcInfoMap[aggDef.TransitionFunction].QualifiedName)
		fmt.Fprintf(predataFile, "\tSTYPE = %s", aggDef.TransitionDataType)

		if aggDef.PreliminaryFunction != 0 {
			fmt.Fprintf(predataFile, ",\n\tPREFUNC = %s", funcInfoMap[aggDef.PreliminaryFunction].QualifiedName)
		}
		if aggDef.FinalFunction != 0 {
			fmt.Fprintf(predataFile, ",\n\tFINALFUNC = %s", funcInfoMap[aggDef.FinalFunction].QualifiedName)
		}
		if aggDef.InitialValue != "" {
			fmt.Fprintf(predataFile, ",\n\tINITCOND = '%s'", aggDef.InitialValue)
		}
		if aggDef.SortOperator != 0 {
			fmt.Fprintf(predataFile, ",\n\tSORTOP = %s", funcInfoMap[aggDef.SortOperator].QualifiedName)
		}

		fmt.Fprintln(predataFile, "\n);")

		if aggDef.Owner != "" {
			fmt.Fprintf(predataFile, "\nALTER AGGREGATE %s(%s) OWNER TO %s;\n", aggFQN, aggDef.IdentArgs, utils.QuoteIdent(aggDef.Owner))
		}
		if aggDef.Comment != "" {
			fmt.Fprintf(predataFile, "\nCOMMENT ON AGGREGATE %s(%s) IS '%s';\n", aggFQN, aggDef.IdentArgs, aggDef.Comment)
		}
	}
}

func PrintCreateCastStatements(predataFile io.Writer, castDefs []QueryCastDefinition) {
	for _, castDef := range castDefs {
		castStr := fmt.Sprintf("CAST (%s AS %s)", castDef.SourceType, castDef.TargetType)
		fmt.Fprintf(predataFile, "\n\nCREATE %s\n", castStr)
		if castDef.FunctionSchema != "" {
			funcFQN := fmt.Sprintf("%s.%s", utils.QuoteIdent(castDef.FunctionSchema), utils.QuoteIdent(castDef.FunctionName))
			fmt.Fprintf(predataFile, "\tWITH FUNCTION %s(%s)", funcFQN, castDef.FunctionArgs)
		} else {
			fmt.Fprintf(predataFile, "\tWITHOUT FUNCTION")
		}
		switch castDef.CastContext {
		case "a":
			fmt.Fprintf(predataFile, "\nAS ASSIGNMENT")
		case "i":
			fmt.Fprintf(predataFile, "\nAS IMPLICIT")
		case "e": // Default case, don't print anything else
		}
		fmt.Fprintln(predataFile, ";")
		if castDef.Comment != "" {
			fmt.Fprintf(predataFile, "\nCOMMENT ON %s IS '%s';\n", castStr, castDef.Comment)
		}
	}
}

/*
 * Because only base types are dependent on functions, we only need to print
 * shell type statements for base types.
 */
func PrintShellTypeStatements(predataFile io.Writer, types []TypeDefinition) {
	fmt.Fprintln(predataFile, "\n")
	for _, typ := range types {
		if typ.Type == "b" {
			typeFQN := utils.MakeFQN(typ.TypeSchema, typ.TypeName)
			fmt.Fprintf(predataFile, "CREATE TYPE %s;\n", typeFQN)
		}
	}
}

func PrintCreateBaseTypeStatements(predataFile io.Writer, types []TypeDefinition) {
	i := 0
	for i < len(types) {
		typ := types[i]
		if typ.Type == "b" {
			typeFQN := utils.MakeFQN(typ.TypeSchema, typ.TypeName)
			fmt.Fprintf(predataFile, "\n\nCREATE TYPE %s (\n", typeFQN)

			fmt.Fprintf(predataFile, "\tINPUT = %s,\n\tOUTPUT = %s", typ.Input, typ.Output)
			if typ.Receive != "-" {
				fmt.Fprintf(predataFile, ",\n\tRECEIVE = %s", typ.Receive)
			}
			if typ.Send != "-" {
				fmt.Fprintf(predataFile, ",\n\tSEND = %s", typ.Send)
			}
			if typ.ModIn != "-" {
				fmt.Fprintf(predataFile, ",\n\tTYPMOD_IN = %s", typ.ModIn)
			}
			if typ.ModOut != "-" {
				fmt.Fprintf(predataFile, ",\n\tTYPMOD_OUT = %s", typ.ModOut)
			}
			if typ.InternalLength > 0 {
				fmt.Fprintf(predataFile, ",\n\tINTERNALLENGTH = %d", typ.InternalLength)
			}
			if typ.IsPassedByValue {
				fmt.Fprintf(predataFile, ",\n\tPASSEDBYVALUE")
			}
			if typ.Alignment != "-" {
				switch typ.Alignment {
				case "d":
					fmt.Fprintf(predataFile, ",\n\tALIGNMENT = double")
				case "i":
					fmt.Fprintf(predataFile, ",\n\tALIGNMENT = int4")
				case "s":
					fmt.Fprintf(predataFile, ",\n\tALIGNMENT = int2")
				case "c": // Default case, don't print anything else
				}
			}
			if typ.Storage != "" {
				switch typ.Storage {
				case "e":
					fmt.Fprintf(predataFile, ",\n\tSTORAGE = extended")
				case "m":
					fmt.Fprintf(predataFile, ",\n\tSTORAGE = main")
				case "x":
					fmt.Fprintf(predataFile, ",\n\tSTORAGE = external")
				case "p": // Default case, don't print anything else
				}
			}
			if typ.DefaultVal != "" {
				fmt.Fprintf(predataFile, ",\n\tDEFAULT = %s", typ.DefaultVal)
			}
			if typ.Element != "-" {
				fmt.Fprintf(predataFile, ",\n\tELEMENT = %s", typ.Element)
			}
			if typ.Delimiter != "" {
				fmt.Fprintf(predataFile, ",\n\tDELIMITER = '%s'", typ.Delimiter)
			}
			fmt.Fprintln(predataFile, "\n);")
			if typ.Comment != "" {
				fmt.Fprintf(predataFile, "\nCOMMENT ON TYPE %s IS '%s';\n", typeFQN, typ.Comment)
			}
			if typ.Owner != "" {
				fmt.Fprintf(predataFile, "\nALTER TYPE %s OWNER TO %s;\n", typeFQN, typ.Owner)
			}
		}
		i++
	}
}

func PrintCreateCompositeAndEnumTypeStatements(predataFile io.Writer, types []TypeDefinition) {
	i := 0
	for i < len(types) {
		typ := types[i]
		if typ.Type == "c" {
			compositeTypes := make([]TypeDefinition, 0)
			/*
			 * Since types is sorted by schema then by type, all TypeDefinitions
			 * for the same composite type are grouped together.  Collect them in
			 * one list to use for printing
			 */
			for {
				if i < len(types) && typ.TypeSchema == types[i].TypeSchema && typ.TypeName == types[i].TypeName {
					compositeTypes = append(compositeTypes, types[i])
					i++
				} else {
					break
				}
			}
			/*
			 * All values except AttName and AttValue will be the same for each TypeDefinition,
			 * so we can grab all other values from the first TypeDefinition in the list.
			 */
			composite := compositeTypes[0]
			typeFQN := utils.MakeFQN(composite.TypeSchema, composite.TypeName)
			fmt.Fprintf(predataFile, "\n\nCREATE TYPE %s AS (\n", typeFQN)
			atts := make([]string, 0)
			for _, composite := range compositeTypes {
				atts = append(atts, fmt.Sprintf("\t%s %s", composite.AttName, composite.AttValue))
			}
			fmt.Fprintf(predataFile, strings.Join(atts, ",\n"))
			fmt.Fprintln(predataFile, "\n);")
			if composite.Comment != "" {
				fmt.Fprintf(predataFile, "\nCOMMENT ON TYPE %s IS '%s';\n", typeFQN, composite.Comment)
			}
			if composite.Owner != "" {
				fmt.Fprintf(predataFile, "\nALTER TYPE %s OWNER TO %s;\n", typeFQN, composite.Owner)
			}
		} else if typ.Type == "e" {
			typeFQN := utils.MakeFQN(typ.TypeSchema, typ.TypeName)
			fmt.Fprintf(predataFile, "\n\nCREATE TYPE %s AS ENUM (\n\t%s\n);\n", typeFQN, typ.EnumLabels)
			if typ.Comment != "" {
				fmt.Fprintf(predataFile, "\nCOMMENT ON TYPE %s IS '%s';\n", typeFQN, typ.Comment)
			}
			if typ.Owner != "" {
				fmt.Fprintf(predataFile, "\nALTER TYPE %s OWNER TO %s;\n", typeFQN, typ.Owner)
			}
			i++

		} else {
			i++
		}
	}
}

/*
 * Functions to print to the global or postdata file instead of, or in addition
 * to, the predata file.
 */

func PrintConnectionString(metadataFile io.Writer, dbname string) {
	fmt.Fprintf(metadataFile, "\\c %s\n", dbname)
}

func PrintSessionGUCs(metadataFile io.Writer, gucs QuerySessionGUCs) {
	fmt.Fprintf(metadataFile, `SET statement_timeout = 0;
SET check_function_bodies = false;
SET client_min_messages = error;
SET client_encoding = '%s';
SET standard_conforming_strings = %s;
SET default_with_oids = %s;
`, gucs.ClientEncoding, gucs.StdConformingStrings, gucs.DefaultWithOids)
}

func PrintCreateDatabaseStatement(globalFile io.Writer) {
	dbname := utils.QuoteIdent(connection.DBName)
	owner := utils.QuoteIdent(GetDatabaseOwner(connection))
	fmt.Fprintf(globalFile, "\n\nCREATE DATABASE %s;", dbname)
	fmt.Fprintf(globalFile, "\nALTER DATABASE %s OWNER TO %s;", dbname, owner)
}

func PrintDatabaseGUCs(globalFile io.Writer, gucs []string, dbname string) {
	for _, guc := range gucs {
		fmt.Fprintf(globalFile, "\nALTER DATABASE %s %s;", dbname, guc)
	}
}
