package end_to_end_test

import (
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/greenplum-db/gp-common-go-libs/dbconn"
	"github.com/greenplum-db/gp-common-go-libs/iohelper"
	"github.com/greenplum-db/gp-common-go-libs/operating"
	"github.com/greenplum-db/gp-common-go-libs/testhelper"
	"github.com/greenplum-db/gpbackup/testutils"
	"github.com/greenplum-db/gpbackup/utils"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"

	"testing"
)

var useOldBackupVersion bool

/* This function is a helper function to execute gpbackup and return a session
 * to allow checking its output.
 */
func gpbackup(gpbackupPath string, backupHelperPath string, args ...string) string {
	if useOldBackupVersion {
		os.Chdir("..")
		output, err := exec.Command("make", "install_helper", fmt.Sprintf("helper_path=%s", backupHelperPath)).CombinedOutput()
		if err != nil {
			fmt.Printf("%s", output)
			Fail(fmt.Sprintf("%v", err))
		}
		os.Chdir("end_to_end")
	}
	args = append([]string{"--dbname", "testdb"}, args...)
	command := exec.Command(gpbackupPath, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		fmt.Printf("%s", output)
		Fail(fmt.Sprintf("%v", err))
	}
	r := regexp.MustCompile(`Backup Timestamp = (\d{14})`)
	return r.FindStringSubmatch(fmt.Sprintf("%s", output))[1]
}

func gprestore(gprestorePath string, restoreHelperPath string, timestamp string, args ...string) []byte {
	if useOldBackupVersion {
		os.Chdir("..")
		output, err := exec.Command("make", "install_helper", fmt.Sprintf("helper_path=%s", restoreHelperPath)).CombinedOutput()
		if err != nil {
			fmt.Printf("%s", output)
			Fail(fmt.Sprintf("%v", err))
		}
		os.Chdir("end_to_end")
	}
	args = append([]string{"--timestamp", timestamp}, args...)
	command := exec.Command(gprestorePath, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		fmt.Printf("%s", output)
		Fail(fmt.Sprintf("%v", err))
	}
	return output
}

func buildAndInstallBinaries() (string, string, string) {
	os.Chdir("..")
	command := exec.Command("make", "build")
	output, err := command.CombinedOutput()
	if err != nil {
		fmt.Printf("%s", output)
		Fail(fmt.Sprintf("%v", err))
	}
	os.Chdir("end_to_end")
	binDir := fmt.Sprintf("%s/go/bin", operating.System.Getenv("HOME"))
	return fmt.Sprintf("%s/gpbackup", binDir), fmt.Sprintf("%s/gpbackup_helper", binDir), fmt.Sprintf("%s/gprestore", binDir)
}

func buildOldBinaries() (string, string) {
	os.Chdir("..")
	err := exec.Command("git", "checkout", "1.0.0").Run()
	Expect(err).ShouldNot(HaveOccurred())
	err = exec.Command("dep", "ensure").Run()
	Expect(err).ShouldNot(HaveOccurred())
	gpbackupOldPath, err := gexec.Build("github.com/greenplum-db/gpbackup", "-tags", "gpbackup", "-ldflags", "-X github.com/greenplum-db/gpbackup/backup.version=1.0.0")
	Expect(err).ShouldNot(HaveOccurred())
	gpbackupHelperOldPath, err := gexec.Build("github.com/greenplum-db/gpbackup", "-tags", "gpbackup_helper", "-ldflags", "-X github.com/greenplum-db/gpbackup/helper.version=1.0.0")
	Expect(err).ShouldNot(HaveOccurred())
	err = exec.Command("git", "checkout", "-").Run()
	Expect(err).ShouldNot(HaveOccurred())
	err = exec.Command("dep", "ensure").Run()
	Expect(err).ShouldNot(HaveOccurred())
	os.Chdir("end_to_end")
	return gpbackupOldPath, gpbackupHelperOldPath
}

func assertDataRestored(conn *dbconn.DBConn, tableToTupleCount map[string]int) {
	for name, numTuples := range tableToTupleCount {
		tupleCount := dbconn.MustSelectString(conn, fmt.Sprintf("SELECT count(*) AS string from %s", name))
		Expect(tupleCount).To(Equal(strconv.Itoa(numTuples)))
	}
}

func assertRelationsCreated(conn *dbconn.DBConn, numTables int) {
	countQuery := `SELECT count(*) AS string FROM pg_class c LEFT JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.relkind IN ('S','v','r') AND n.nspname IN ('public', 'schema2');`
	tableCount := dbconn.MustSelectString(conn, countQuery)
	Expect(tableCount).To(Equal(strconv.Itoa(numTables)))
}

func copyPluginToAllHosts(conn *dbconn.DBConn, pluginPath string) {
	hostnameQuery := `SELECT DISTINCT hostname AS string FROM gp_segment_configuration WHERE content != -1`
	hostnames := dbconn.MustSelectStringSlice(conn, hostnameQuery)
	for _, hostname := range hostnames {
		pluginDir, _ := filepath.Split(pluginPath)
		output, err := exec.Command("ssh", hostname, fmt.Sprintf("mkdir -p %s", pluginDir)).CombinedOutput()
		if err != nil {
			fmt.Printf("%s", output)
			Fail(fmt.Sprintf("%v", err))
		}
		output, err = exec.Command("scp", pluginPath, fmt.Sprintf("%s:%s", hostname, pluginPath)).CombinedOutput()
		if err != nil {
			fmt.Printf("%s", output)
			Fail(fmt.Sprintf("%v", err))
		}
	}
}

func TestEndToEnd(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "EndToEnd Suite")
}

var _ = Describe("backup end to end integration tests", func() {

	var backupConn, restoreConn *dbconn.DBConn
	var gpbackupPath, backupHelperPath, restoreHelperPath, gprestorePath string
	BeforeSuite(func() {
		// This is used to run tests from gpbackup 1.0.0 to gprestore latest
		useOldBackupVersion = os.Getenv("USE_OLD_BACKUP_VERSION") == "true"
		var err error
		testhelper.SetupTestLogger()
		exec.Command("dropdb", "testdb").Run()
		exec.Command("dropdb", "restoredb").Run()

		err = exec.Command("createdb", "testdb").Run()
		if err != nil {
			Fail(fmt.Sprintf("Could not create testdb: %v", err))
		}
		err = exec.Command("createdb", "restoredb").Run()
		if err != nil {
			Fail(fmt.Sprintf("Could not create restoredb: %v", err))
		}
		backupConn = dbconn.NewDBConnFromEnvironment("testdb")
		backupConn.MustConnect(1)
		restoreConn = dbconn.NewDBConnFromEnvironment("restoredb")
		restoreConn.MustConnect(1)
		testutils.ExecuteSQLFile(backupConn, "test_tables_ddl.sql")
		testutils.ExecuteSQLFile(backupConn, "test_tables_data.sql")
		if useOldBackupVersion {
			_, restoreHelperPath, gprestorePath = buildAndInstallBinaries()
			gpbackupPath, backupHelperPath = buildOldBinaries()
		} else {
			gpbackupPath, backupHelperPath, gprestorePath = buildAndInstallBinaries()
			restoreHelperPath = backupHelperPath
		}
	})
	AfterSuite(func() {
		if backupConn != nil {
			backupConn.Close()
		}
		if restoreConn != nil {
			restoreConn.Close()
		}
		gexec.CleanupBuildArtifacts()
		err := exec.Command("dropdb", "testdb").Run()
		if err != nil {
			fmt.Printf("Could not drop testdb: %v\n", err)
		}
		err = exec.Command("dropdb", "restoredb").Run()
		if err != nil {
			fmt.Printf("Could not drop restoredb: %v\n", err)
		}
	})

	Describe("end to end gpbackup and gprestore tests", func() {
		var publicSchemaTupleCounts, schema2TupleCounts map[string]int

		BeforeEach(func() {
			testhelper.AssertQueryRuns(restoreConn, "DROP SCHEMA IF EXISTS schema2 CASCADE; DROP SCHEMA public CASCADE; CREATE SCHEMA public;")
			publicSchemaTupleCounts = map[string]int{
				"public.foo":   40000,
				"public.holds": 50000,
				"public.sales": 13,
			}
			schema2TupleCounts = map[string]int{
				"schema2.returns": 6,
				"schema2.foo2":    0,
				"schema2.foo3":    100,
				"schema2.ao1":     1000,
				"schema2.ao2":     1000,
			}
		})
		Describe("Backup include filtering", func() {
			It("runs gpbackup and gprestore with include-schema backup flag and compression level", func() {
				timestamp := gpbackup(gpbackupPath, backupHelperPath, "--include-schema", "public", "--compression-level", "2")
				gprestore(gprestorePath, restoreHelperPath, timestamp, "--redirect-db", "restoredb")

				assertRelationsCreated(restoreConn, 19)
				assertDataRestored(restoreConn, publicSchemaTupleCounts)
			})
			It("runs gpbackup and gprestore with include-table backup flag", func() {
				if useOldBackupVersion {
					Skip("Feature not supported in gpbackup 1.0.0")
				}
				timestamp := gpbackup(gpbackupPath, backupHelperPath, "--include-table", "public.foo", "--include-table", "public.sales", "--include-table", "public.myseq1", "--include-table", "public.myview1")
				gprestore(gprestorePath, restoreHelperPath, timestamp, "--redirect-db", "restoredb")

				assertRelationsCreated(restoreConn, 16)
				assertDataRestored(restoreConn, map[string]int{"public.foo": 40000})

				os.Remove("/tmp/include-tables.txt")
			})
			It("runs gpbackup and gprestore with include-table-file backup flag", func() {
				if useOldBackupVersion {
					Skip("Feature not supported in gpbackup 1.0.0")
				}
				includeFile := iohelper.MustOpenFileForWriting("/tmp/include-tables.txt")
				utils.MustPrintln(includeFile, "public.sales\npublic.foo\npublic.myseq1\npublic.myview1")
				timestamp := gpbackup(gpbackupPath, backupHelperPath, "--include-table-file", "/tmp/include-tables.txt")
				gprestore(gprestorePath, restoreHelperPath, timestamp, "--redirect-db", "restoredb")

				assertRelationsCreated(restoreConn, 16)
				assertDataRestored(restoreConn, map[string]int{"public.sales": 13, "public.foo": 40000})

				os.Remove("/tmp/include-tables.txt")
			})

		})
		Describe("Restore include filtering", func() {
			It("runs gpbackup and gprestore with include-schema restore flag", func() {
				backupdir := "/tmp/include_schema"
				timestamp := gpbackup(gpbackupPath, backupHelperPath, "--backup-dir", backupdir)
				gprestore(gprestorePath, restoreHelperPath, timestamp, "--redirect-db", "restoredb", "--backup-dir", backupdir, "--include-schema", "schema2")

				assertRelationsCreated(restoreConn, 17)
				assertDataRestored(restoreConn, schema2TupleCounts)

				os.RemoveAll(backupdir)
			})
			It("runs gpbackup and gprestore with include-table restore flag", func() {
				timestamp := gpbackup(gpbackupPath, backupHelperPath)
				gprestore(gprestorePath, restoreHelperPath, timestamp, "--redirect-db", "restoredb", "--include-table", "public.foo", "--include-table", "public.sales", "--include-table", "public.myseq1", "--include-table", "public.myview1")

				assertRelationsCreated(restoreConn, 16)
				assertDataRestored(restoreConn, map[string]int{"public.sales": 13, "public.foo": 40000})

				os.Remove("/tmp/include-tables.txt")
			})
			It("runs gpbackup and gprestore with include-table-file restore flag", func() {
				includeFile := iohelper.MustOpenFileForWriting("/tmp/include-tables.txt")
				utils.MustPrintln(includeFile, "public.sales\npublic.foo\npublic.myseq1\npublic.myview1")
				backupdir := "/tmp/include_table_file"
				timestamp := gpbackup(gpbackupPath, backupHelperPath, "--backup-dir", backupdir)
				gprestore(gprestorePath, restoreHelperPath, timestamp, "--redirect-db", "restoredb", "--backup-dir", backupdir, "--include-table-file", "/tmp/include-tables.txt")

				assertRelationsCreated(restoreConn, 16)
				assertDataRestored(restoreConn, map[string]int{"public.sales": 13, "public.foo": 40000})

				os.RemoveAll(backupdir)
				os.Remove("/tmp/include-tables.txt")
			})
		})
		Describe("Backup exclude filtering", func() {
			It("runs gpbackup and gprestore with exclude-schema backup flag", func() {
				timestamp := gpbackup(gpbackupPath, backupHelperPath, "--exclude-schema", "public")
				gprestore(gprestorePath, restoreHelperPath, timestamp, "--redirect-db", "restoredb")

				assertRelationsCreated(restoreConn, 17)
				assertDataRestored(restoreConn, schema2TupleCounts)
			})
			It("runs gpbackup and gprestore with exclude-table backup flag", func() {
				if useOldBackupVersion {
					Skip("Feature not supported in gpbackup 1.0.0")
				}
				timestamp := gpbackup(gpbackupPath, backupHelperPath, "--exclude-table", "schema2.foo2", "--exclude-table", "schema2.returns", "--exclude-table", "public.myseq1", "--exclude-table", "public.myview1")
				gprestore(gprestorePath, restoreHelperPath, timestamp, "--redirect-db", "restoredb")

				assertRelationsCreated(restoreConn, 20)
				assertDataRestored(restoreConn, map[string]int{"schema2.foo3": 100, "public.foo": 40000, "public.holds": 50000, "public.sales": 13})

				os.Remove("/tmp/exclude-tables.txt")
			})
			It("runs gpbackup and gprestore with exclude-table-file backup flag", func() {
				if useOldBackupVersion {
					Skip("Feature not supported in gpbackup 1.0.0")
				}
				excludeFile := iohelper.MustOpenFileForWriting("/tmp/exclude-tables.txt")
				utils.MustPrintln(excludeFile, "schema2.foo2\nschema2.returns\npublic.sales\npublic.myseq1\npublic.myview1")
				timestamp := gpbackup(gpbackupPath, backupHelperPath, "--exclude-table-file", "/tmp/exclude-tables.txt")
				gprestore(gprestorePath, restoreHelperPath, timestamp, "--redirect-db", "restoredb")

				assertRelationsCreated(restoreConn, 7)
				assertDataRestored(restoreConn, map[string]int{"schema2.foo3": 100, "public.foo": 40000, "public.holds": 50000})

				os.Remove("/tmp/exclude-tables.txt")
			})
		})
		Describe("Restore exclude filtering", func() {
			It("runs gpbackup and gprestore with exclude-schema restore flag", func() {
				timestamp := gpbackup(gpbackupPath, backupHelperPath)
				gprestore(gprestorePath, restoreHelperPath, timestamp, "--redirect-db", "restoredb", "--exclude-schema", "public")

				assertRelationsCreated(restoreConn, 17)
				assertDataRestored(restoreConn, schema2TupleCounts)
			})
			It("runs gpbackup and gprestore with exclude-table restore flag", func() {
				timestamp := gpbackup(gpbackupPath, backupHelperPath)
				gprestore(gprestorePath, restoreHelperPath, timestamp, "--redirect-db", "restoredb", "--exclude-table", "schema2.foo2", "--exclude-table", "schema2.returns", "--exclude-table", "public.myseq1", "--exclude-table", "public.myview1")

				assertRelationsCreated(restoreConn, 20)
				assertDataRestored(restoreConn, map[string]int{"schema2.foo3": 100, "public.foo": 40000, "public.holds": 50000, "public.sales": 13})

				os.Remove("/tmp/exclude-tables.txt")
			})
			It("runs gpbackup and gprestore with exclude-table-file restore flag", func() {
				includeFile := iohelper.MustOpenFileForWriting("/tmp/exclude-tables.txt")
				utils.MustPrintln(includeFile, "schema2.foo2\nschema2.returns\npublic.myseq1\npublic.myview1")
				backupdir := "/tmp/exclude_table_file"
				timestamp := gpbackup(gpbackupPath, backupHelperPath, "--backup-dir", backupdir)
				gprestore(gprestorePath, restoreHelperPath, timestamp, "--redirect-db", "restoredb", "--backup-dir", backupdir, "--exclude-table-file", "/tmp/exclude-tables.txt")

				assertRelationsCreated(restoreConn, 20)
				assertDataRestored(restoreConn, map[string]int{"public.sales": 13, "public.foo": 40000})

				os.RemoveAll(backupdir)
				os.Remove("/tmp/exclude-tables.txt")
			})
		})
		Describe("Single data file", func() {
			It("runs gpbackup and gprestore with single-data-file flag", func() {
				backupdir := "/tmp/single_data_file"
				timestamp := gpbackup(gpbackupPath, backupHelperPath, "--single-data-file", "--backup-dir", backupdir)
				gprestore(gprestorePath, restoreHelperPath, timestamp, "--redirect-db", "restoredb", "--backup-dir", backupdir)

				assertRelationsCreated(restoreConn, 36)
				assertDataRestored(restoreConn, publicSchemaTupleCounts)
				assertDataRestored(restoreConn, schema2TupleCounts)

				os.RemoveAll(backupdir)
			})

			It("runs gpbackup and gprestore with single-data-file flag without compression", func() {
				backupdir := "/tmp/single_data_file"
				timestamp := gpbackup(gpbackupPath, backupHelperPath, "--single-data-file", "--backup-dir", backupdir, "--no-compression")
				gprestore(gprestorePath, restoreHelperPath, timestamp, "--redirect-db", "restoredb", "--backup-dir", backupdir)

				assertRelationsCreated(restoreConn, 36)
				assertDataRestored(restoreConn, publicSchemaTupleCounts)
				assertDataRestored(restoreConn, schema2TupleCounts)

				os.RemoveAll(backupdir)
			})

			It("runs gpbackup and gprestore with plugin, single-data-file, and no-compression", func() {
				if useOldBackupVersion {
					Skip("Feature not supported in gpbackup 1.0.0")
				}

				pluginDir := "/tmp/plugin_dest"
				pluginExecutablePath := fmt.Sprintf("%s/go/src/github.com/greenplum-db/gpbackup/plugins/example_plugin.sh", os.Getenv("HOME"))
				copyPluginToAllHosts(backupConn, pluginExecutablePath)
				pluginConfigPath := fmt.Sprintf("%s/go/src/github.com/greenplum-db/gpbackup/plugins/example_plugin_config.yaml", os.Getenv("HOME"))

				timestamp := gpbackup(gpbackupPath, backupHelperPath, "--single-data-file", "--no-compression", "--plugin-config", pluginConfigPath)
				gprestore(gprestorePath, restoreHelperPath, timestamp, "--redirect-db", "restoredb", "--plugin-config", pluginConfigPath)

				assertRelationsCreated(restoreConn, 36)
				assertDataRestored(restoreConn, publicSchemaTupleCounts)
				assertDataRestored(restoreConn, schema2TupleCounts)

				os.RemoveAll(pluginDir)
			})
			It("runs gpbackup and gprestore with plugin and single-data-file", func() {
				if useOldBackupVersion {
					Skip("Feature not supported in gpbackup 1.0.0")
				}
				pluginDir := "/tmp/plugin_dest"
				pluginExecutablePath := fmt.Sprintf("%s/go/src/github.com/greenplum-db/gpbackup/plugins/example_plugin.sh", os.Getenv("HOME"))
				copyPluginToAllHosts(backupConn, pluginExecutablePath)
				pluginConfigPath := fmt.Sprintf("%s/go/src/github.com/greenplum-db/gpbackup/plugins/example_plugin_config.yaml", os.Getenv("HOME"))

				timestamp := gpbackup(gpbackupPath, backupHelperPath, "--single-data-file", "--plugin-config", pluginConfigPath)
				gprestore(gprestorePath, restoreHelperPath, timestamp, "--redirect-db", "restoredb", "--plugin-config", pluginConfigPath)

				assertRelationsCreated(restoreConn, 36)
				assertDataRestored(restoreConn, publicSchemaTupleCounts)
				assertDataRestored(restoreConn, schema2TupleCounts)

				os.RemoveAll(pluginDir)
			})
			It("runs gpbackup and gprestore with include-table-file restore flag with a single data file", func() {
				includeFile := iohelper.MustOpenFileForWriting("/tmp/include-tables.txt")
				utils.MustPrintln(includeFile, "public.sales\npublic.foo")
				backupdir := "/tmp/include_table_file"
				timestamp := gpbackup(gpbackupPath, backupHelperPath, "--backup-dir", backupdir, "--single-data-file")
				gprestore(gprestorePath, restoreHelperPath, timestamp, "--redirect-db", "restoredb", "--backup-dir", backupdir, "--include-table-file", "/tmp/include-tables.txt")
				assertRelationsCreated(restoreConn, 14)
				assertDataRestored(restoreConn, map[string]int{"public.sales": 13, "public.foo": 40000})

				os.RemoveAll(backupdir)
				os.Remove("/tmp/include-tables.txt")
			})
			It("runs gpbackup and gprestore with include-schema restore flag with a single data file", func() {
				backupdir := "/tmp/include_schema"
				timestamp := gpbackup(gpbackupPath, backupHelperPath, "--backup-dir", backupdir, "--single-data-file")
				gprestore(gprestorePath, restoreHelperPath, timestamp, "--redirect-db", "restoredb", "--backup-dir", backupdir, "--include-schema", "schema2")

				assertRelationsCreated(restoreConn, 17)
				assertDataRestored(restoreConn, schema2TupleCounts)

				os.RemoveAll(backupdir)
			})
			It("runs gpbackup and gprestore on database with all objects", func() {
				testhelper.AssertQueryRuns(backupConn, "DROP SCHEMA IF EXISTS schema2 CASCADE; DROP SCHEMA public CASCADE; CREATE SCHEMA public; DROP PROCEDURAL LANGUAGE IF EXISTS plpythonu;")
				defer testutils.ExecuteSQLFile(backupConn, "test_tables_data.sql")
				defer testutils.ExecuteSQLFile(backupConn, "test_tables_ddl.sql")
				defer testhelper.AssertQueryRuns(backupConn, "DROP SCHEMA IF EXISTS schema2 CASCADE; DROP SCHEMA public CASCADE; CREATE SCHEMA public; DROP PROCEDURAL LANGUAGE IF EXISTS plpythonu;")
				testutils.ExecuteSQLFile(backupConn, "gpdb4_objects.sql")
				if backupConn.Version.AtLeast("5") {
					testutils.ExecuteSQLFile(backupConn, "gpdb5_objects.sql")
				}
				if backupConn.Version.AtLeast("6") {
					testutils.ExecuteSQLFile(backupConn, "gpdb6_objects.sql")
				}
				timestamp := gpbackup(gpbackupPath, backupHelperPath, "--leaf-partition-data", "--single-data-file")
				gprestore(gprestorePath, restoreHelperPath, timestamp, "--redirect-db", "restoredb")

			})
		})
		Describe("Incremental", func() {
			It("restores from an incremental backup", func() {
				fullBackupTimestamp := gpbackup(gpbackupPath, backupHelperPath)

				testhelper.AssertQueryRuns(backupConn, "INSERT into schema2.ao1 values(1001)")
				defer testhelper.AssertQueryRuns(backupConn, "DELETE from schema2.ao1 where i=1001")
				incremental1Timestamp := gpbackup(gpbackupPath, backupHelperPath, "--incremental", fullBackupTimestamp)

				testhelper.AssertQueryRuns(backupConn, "INSERT into schema2.ao1 values(1002)")
				defer testhelper.AssertQueryRuns(backupConn, "DELETE from schema2.ao1 where i=1002")
				incremental2Timestamp := gpbackup(gpbackupPath, backupHelperPath, "--incremental", incremental1Timestamp)

				gprestore(gprestorePath, restoreHelperPath, incremental2Timestamp, "--redirect-db", "restoredb")

				assertRelationsCreated(restoreConn, 36)
				assertDataRestored(restoreConn, publicSchemaTupleCounts)
				schema2TupleCounts["schema2.ao1"] = 1002
				assertDataRestored(restoreConn, schema2TupleCounts)
			})
		})
		It("runs gpbackup and gprestore without redirecting restore to another db", func() {
			timestamp := gpbackup(gpbackupPath, backupHelperPath)
			backupConn.Close()
			err := exec.Command("dropdb", "testdb").Run()
			if err != nil {
				Fail(fmt.Sprintf("%v", err))
			}
			gprestore(gprestorePath, restoreHelperPath, timestamp, "--create-db")
			backupConn = dbconn.NewDBConnFromEnvironment("testdb")
			backupConn.MustConnect(1)
		})
		It("runs basic gpbackup and gprestore with metadata and data-only flags", func() {
			timestamp := gpbackup(gpbackupPath, backupHelperPath, "--metadata-only")
			timestamp2 := gpbackup(gpbackupPath, backupHelperPath, "--data-only")
			gprestore(gprestorePath, restoreHelperPath, timestamp, "--redirect-db", "restoredb")
			assertDataRestored(restoreConn, map[string]int{"public.foo": 0, "schema2.foo3": 0})
			assertRelationsCreated(restoreConn, 36)
			gprestore(gprestorePath, restoreHelperPath, timestamp2, "--redirect-db", "restoredb")

			assertDataRestored(restoreConn, publicSchemaTupleCounts)
			assertDataRestored(restoreConn, schema2TupleCounts)
		})
		It("runs gpbackup and gprestore with metadata-only backup flag", func() {
			timestamp := gpbackup(gpbackupPath, backupHelperPath, "--metadata-only")
			gprestore(gprestorePath, restoreHelperPath, timestamp, "--redirect-db", "restoredb")

			assertDataRestored(restoreConn, map[string]int{"public.foo": 0, "schema2.foo3": 0})
			assertRelationsCreated(restoreConn, 36)
		})
		It("runs gpbackup and gprestore with data-only backup flag", func() {
			testutils.ExecuteSQLFile(restoreConn, "test_tables_ddl.sql")

			timestamp := gpbackup(gpbackupPath, backupHelperPath, "--data-only")
			gprestore(gprestorePath, restoreHelperPath, timestamp, "--redirect-db", "restoredb")

			assertDataRestored(restoreConn, publicSchemaTupleCounts)
			assertDataRestored(restoreConn, schema2TupleCounts)
		})

		It("runs gpbackup and gprestore with the data-only restore flag", func() {
			testutils.ExecuteSQLFile(restoreConn, "test_tables_ddl.sql")
			timestamp := gpbackup(gpbackupPath, backupHelperPath)
			gprestore(gprestorePath, restoreHelperPath, timestamp, "--redirect-db", "restoredb", "--data-only")

			assertDataRestored(restoreConn, publicSchemaTupleCounts)
			assertDataRestored(restoreConn, schema2TupleCounts)
		})
		It("runs gpbackup and gprestore with the metadata-only restore flag", func() {
			timestamp := gpbackup(gpbackupPath, backupHelperPath)
			gprestore(gprestorePath, restoreHelperPath, timestamp, "--redirect-db", "restoredb", "--metadata-only")

			assertDataRestored(restoreConn, map[string]int{"public.foo": 0, "schema2.foo3": 0})
			assertRelationsCreated(restoreConn, 36)
		})
		It("runs gpbackup and gprestore with leaf-partition-data and backupdir flags", func() {
			backupdir := "/tmp/leaf_partition_data"
			timestamp := gpbackup(gpbackupPath, backupHelperPath, "--leaf-partition-data", "--backup-dir", backupdir)
			output := gprestore(gprestorePath, restoreHelperPath, timestamp, "--redirect-db", "restoredb", "--backup-dir", backupdir)
			Expect(strings.Contains(string(output), "Tables restored:  30 / 30")).To(BeTrue())

			assertDataRestored(restoreConn, publicSchemaTupleCounts)
			assertDataRestored(restoreConn, schema2TupleCounts)

			os.RemoveAll(backupdir)
		})
		It("runs gpbackup and gprestore with no-compression flag", func() {
			backupdir := "/tmp/no_compression"
			timestamp := gpbackup(gpbackupPath, backupHelperPath, "--no-compression", "--backup-dir", backupdir)
			gprestore(gprestorePath, restoreHelperPath, timestamp, "--redirect-db", "restoredb", "--backup-dir", backupdir)
			configFile, _ := filepath.Glob(filepath.Join(backupdir, "*-1/backups/*", timestamp, "*config.yaml"))
			contents, _ := ioutil.ReadFile(configFile[0])

			Expect(strings.Contains(string(contents), "compressed: false")).To(BeTrue())
			assertRelationsCreated(restoreConn, 36)
			assertDataRestored(restoreConn, publicSchemaTupleCounts)
			assertDataRestored(restoreConn, schema2TupleCounts)

			os.RemoveAll(backupdir)
		})
		It("runs gpbackup and gprestore with with-stats flag", func() {
			backupdir := "/tmp/with_stats"
			timestamp := gpbackup(gpbackupPath, backupHelperPath, "--with-stats", "--backup-dir", backupdir)
			files, _ := filepath.Glob(filepath.Join(backupdir, "*-1/backups/*", timestamp, "*statistics.sql"))

			Expect(len(files)).To(Equal(1))
			output := gprestore(gprestorePath, restoreHelperPath, timestamp, "--redirect-db", "restoredb", "--with-stats", "--backup-dir", backupdir)

			Expect(strings.Contains(string(output), "Query planner statistics restore complete")).To(BeTrue())
			assertDataRestored(restoreConn, publicSchemaTupleCounts)
			assertDataRestored(restoreConn, schema2TupleCounts)

			os.RemoveAll(backupdir)
		})
		It("runs gpbackup and gprestore with jobs flag", func() {
			if useOldBackupVersion {
				Skip("Feature not supported in gpbackup 1.0.0")
			}
			backupdir := "/tmp/parallel"
			timestamp := gpbackup(gpbackupPath, backupHelperPath, "--backup-dir", backupdir, "--jobs", "4")
			gprestore(gprestorePath, restoreHelperPath, timestamp, "--redirect-db", "restoredb", "--backup-dir", backupdir, "--jobs", "4")

			assertRelationsCreated(restoreConn, 36)
			assertDataRestored(restoreConn, schema2TupleCounts)
			assertDataRestored(restoreConn, publicSchemaTupleCounts)

			os.RemoveAll(backupdir)
		})
		It("runs gpbackup and sends a SIGINT to ensure cleanup functions successfully", func() {
			backupdir := "/tmp/signals"
			args := []string{"--dbname", "testdb", "--backup-dir", backupdir, "--single-data-file", "--verbose"}
			cmd := exec.Command(gpbackupPath, args...)
			go func() {
				/*
				 * We use a random delay for the sleep in this test (between
				 * 0.5s and 1.5s) so that gpbackup will be interrupted at a
				 * different point in the backup process every time to help
				 * catch timing issues with the cleanup.
				 */
				rng := rand.New(rand.NewSource(time.Now().UnixNano()))
				time.Sleep(time.Duration(rng.Intn(1000)+500) * time.Millisecond)
				cmd.Process.Signal(os.Interrupt)
			}()
			output, _ := cmd.CombinedOutput()
			stdout := string(output)

			Expect(stdout).To(ContainSubstring("Received a termination signal, aborting backup process"))
			Expect(stdout).To(ContainSubstring("Cleanup complete"))
			Expect(stdout).To(Not(ContainSubstring("CRITICAL")))

			os.RemoveAll(backupdir)
		})
		It("runs gprestore and sends a SIGINT to ensure cleanup functions successfully", func() {
			backupdir := "/tmp/signals"
			timestamp := gpbackup(gpbackupPath, backupHelperPath, "--backup-dir", backupdir, "--single-data-file")
			args := []string{"--timestamp", timestamp, "--redirect-db", "restoredb", "--backup-dir", backupdir, "--include-schema", "schema2", "--verbose"}
			cmd := exec.Command(gprestorePath, args...)
			go func() {
				/*
				 * We use a random delay for the sleep in this test (between
				 * 0.5s and 1.5s) so that gprestore will be interrupted at a
				 * different point in the backup process every time to help
				 * catch timing issues with the cleanup.
				 */
				rng := rand.New(rand.NewSource(time.Now().UnixNano()))
				time.Sleep(time.Duration(rng.Intn(1000)+500) * time.Millisecond)
				cmd.Process.Signal(os.Interrupt)
			}()
			output, _ := cmd.CombinedOutput()
			stdout := string(output)

			Expect(stdout).To(ContainSubstring("Received a termination signal, aborting restore process"))
			Expect(stdout).To(ContainSubstring("Cleanup complete"))
			Expect(stdout).To(Not(ContainSubstring("CRITICAL")))

			os.RemoveAll(backupdir)
		})
		It("runs example_plugin.sh with plugin_test_bench", func() {
			if useOldBackupVersion {
				Skip("Feature not supported in gpbackup 1.0.0")
			}
			pluginsDir := fmt.Sprintf("%s/go/src/github.com/greenplum-db/gpbackup/plugins", os.Getenv("HOME"))
			copyPluginToAllHosts(backupConn, fmt.Sprintf("%s/example_plugin.sh", pluginsDir))
			output, err := exec.Command("bash", "-c", fmt.Sprintf("%s/plugin_test_bench.sh %s/example_plugin.sh %s/example_plugin_config.yaml", pluginsDir, pluginsDir, pluginsDir)).CombinedOutput()
			if err != nil {
				fmt.Printf("%s", output)
				Fail(fmt.Sprintf("%v", err))
			}

			os.RemoveAll("/tmp/plugin_dest")
		})
		It("runs gpbackup with --version flag", func() {
			if useOldBackupVersion {
				Skip("This test is not needed for gpbackup 1.0.0")
			}
			output, err := exec.Command(gpbackupPath, "--version").CombinedOutput()
			if err != nil {
				fmt.Printf("%s", output)
				Fail(fmt.Sprintf("%v", err))
			}
			Expect(string(output)).To(MatchRegexp(`gpbackup version \w+`))
		})
		It("runs gprestore with --version flag", func() {
			output, err := exec.Command(gprestorePath, "--version").CombinedOutput()
			if err != nil {
				fmt.Printf("%s", output)
				Fail(fmt.Sprintf("%v", err))
			}
			Expect(string(output)).To(MatchRegexp(`gprestore version \w+`))
		})

	})
})
