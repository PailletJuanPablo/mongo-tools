package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mongodb/mongo-tools/release/aws"
	"github.com/mongodb/mongo-tools/release/download"
	"github.com/mongodb/mongo-tools/release/env"
	"github.com/mongodb/mongo-tools/release/evergreen"
	"github.com/mongodb/mongo-tools/release/platform"
	"github.com/mongodb/mongo-tools/release/version"
)

// These are the binaries that are part of mongo-tools, relative
// to the location of this go file.
var binaries = []string{
	"bsondump",
	"mongodump",
	"mongoexport",
	"mongofiles",
	"mongoimport",
	"mongorestore",
	"mongostat",
	"mongotop",
}

var staticFiles = []string{
	"LICENSE.md",
	"README.md",
	"THIRD-PARTY-NOTICES",
}

func main() {
	// don't prefix log messages with anything
	log.SetFlags(0)

	var cmd string
	var v version.Version
	var err error

	switch len(os.Args) {
	case 1:
		log.Fatal("please provide a subcommand")
	case 2:
		cmd = os.Args[1]
		v, err = version.GetCurrent()
		if err != nil {
			log.Fatalf("failed to get version: %v", err)
		}

	case 3:
		cmd = os.Args[1]
		v, err = version.GetFromRev(os.Args[2])
		if err != nil {
			log.Fatalf("failed to get version: %v", err)
		}
	default:
		log.Fatalf("expected one or two arguments, got %d", len(os.Args))
	}

	switch cmd {
	case "build-archive":
		buildArchive()
	case "build-packages":
		buildMSI()
		buildLinuxPackages()
	case "get-version":
		fmt.Println(v)
	case "list-deps":
		listLinuxDeps()
	case "upload-release":
		uploadRelease(v)
	default:
		log.Fatalf("unknown subcommand '%s'", cmd)
	}
}

func check(err error, format ...interface{}) {
	if err == nil {
		return
	}
	msg := err.Error()
	if len(format) != 0 {
		task := fmt.Sprintf(format[0].(string), format[1:]...)
		msg = fmt.Sprintf("'%s' failed: %v", task, err)
	}
	log.Fatal(msg)
}

func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.Output()
	if err != nil {
		if exerr, ok := err.(*exec.ExitError); ok {
			err = fmt.Errorf("ExitError: %v. Stderr: %q", err, string(exerr.Stderr))
		}
	}
	return strings.TrimSpace(string(out)), err
}

func isTaggedRelease(rev string) bool {
	_, err := run("git", "describe", "--exact", rev)
	return err == nil
}

func getReleaseName() string {
	p, err := platform.GetFromEnv()
	check(err, "get platform")

	v, err := version.GetCurrent()
	check(err, "get version")

	return fmt.Sprintf(
		"mongodb-database-tools-%s-%s-%s",
		p.Name, p.Arch, v,
	)
}

func buildArchive() {
	pf, err := platform.GetFromEnv()
	check(err, "get platform")
	if pf.OS == platform.OSWindows {
		buildZip()
	} else {
		buildTarball()
	}
}

func listLinuxDeps() {
	pf, err := platform.GetFromEnv()
	check(err, "get platform")

	if pf.OS != platform.OSLinux {
		return
	}

	check(err, "get platform")
	libraryPaths := getLibraryPaths()
	deps := make(map[string]struct{})

	switch pf.Pkg {
	case platform.PkgRPM:
		for _, libPath := range libraryPaths {
			out, err := run("rpm", "-q", "--whatprovides", libPath)
			check(err, "rpm -q --whatprovides "+libPath+": "+out)
			deps[strings.Trim(out, " \t\n")] = struct{}{}
		}
	case platform.PkgDeb:
		for _, libPath := range libraryPaths {
			out, err := run("dpkg", "-S", libPath)
			check(err, "dpkg -S "+libPath+": "+out)
			sp := strings.Split(out, ":")
			deps[strings.Trim(sp[0], " \t\n")] = struct{}{}
		}
	default:
		log.Fatalf("linux platform %q is neither deb nor rpm based", pf.Name)
	}

	orderedDeps := make([]string, 0, len(deps))
	for dep := range deps {
		orderedDeps = append(orderedDeps, dep)
	}
	sort.Strings(orderedDeps)
	for _, dep := range orderedDeps {
		log.Printf("%s\n", dep)
	}
}

func getLibraryPaths() []string {
	out, err := run("ldd", filepath.Join("bin", "mongodump"))
	check(err, "ldd\n"+out)

	ret := []string{}
	for _, line := range strings.Split(out, "\n") {
		sp := strings.Split(line, "=>")
		if len(sp) < 2 {
			continue
		}
		sp = strings.Split(sp[1], "(")
		libPath := strings.Trim(sp[0], " \t")
		if libPath != "" {
			ret = append(ret, libPath)
		}
	}
	return ret
}

func buildLinuxPackages() {
	pf, err := platform.GetFromEnv()
	check(err, "get platform")
	if pf.OS != platform.OSLinux {
		return
	}

	switch pf.Pkg {
	case platform.PkgRPM:
		buildRPM()
	case platform.PkgDeb:
		buildDeb()
	default:
		log.Fatalf("found linux platform with no Pkg value: %+v", pf)
	}
}

func buildRPM() {
	mdt := "mongodb-database-tools"
	home := os.Getenv("HOME")

	// set up build working directory.
	cdBack := useWorkingDir("rpm_build")
	// we'll want to go back to the original directory, just in case.
	defer cdBack()

	// The goal here is to set up  directory with the following structure:
	// rpmbuild/
	// |----- SOURCES/
	// |         |----- mongodb-database-tools.tar.gz:
	//                       |
	//                      mongodb-database-tools/
	//                               |------ usr/
	//                               |-- bin/
	//                               |    |--- bsondump
	//                               |    |--- mongo*
	//                               |-- share/
	//                                      |---- doc/
	//                                             |----- mongodb-database-tools/
	//                                                              |--- staticFiles

	// create tar file
	log.Printf("tarring necessary files\n")
	createTar := func() {
		staticFilesPath := ".."
		binariesPath := filepath.Join("..", "bin")
		sources := filepath.Join(home, "rpmbuild", "SOURCES")
		check(os.MkdirAll(sources, os.ModePerm), "create "+sources)
		archiveFile, err := os.Create(filepath.Join(sources, mdt+".tar.gz"))
		check(err, "create archive file")
		defer archiveFile.Close()

		gw := gzip.NewWriter(archiveFile)
		defer gw.Close()

		tw := tar.NewWriter(gw)
		defer tw.Close()

		for _, name := range staticFiles {
			log.Printf("adding %s to tarball\n", name)
			src := filepath.Join(staticFilesPath, name)
			dst := filepath.Join(mdt, "usr", "share", "doc", mdt, name)
			addToTarball(tw, dst, src)
		}

		for _, name := range binaries {
			log.Printf("adding %s to tarball\n", name)
			src := filepath.Join(binariesPath, name)
			dst := filepath.Join(mdt, "usr", "bin", name)
			addToTarball(tw, dst, src)
		}
	}
	createTar()

	pf, err := platform.GetFromEnv()
	check(err, "get platform")
	specFile := mdt + ".spec"

	v, err := version.GetCurrent()
	check(err, "get version")

	rpmVersion := v.StringWithoutPre()
	rpmRelease := v.RPMRelease()

	createSpecFile := func() {
		log.Printf("create spec file\n")
		f, err := os.Create(specFile)
		check(err, "create spec")
		defer f.Close()

		// get the control file content.
		contentBytes, err := ioutil.ReadFile(filepath.Join("..", "installer", "rpm", specFile))
		content := string(contentBytes)
		check(err, "reading spec file content")
		content = strings.Replace(content, "@TOOLS_VERSION@", rpmVersion, -1)
		content = strings.Replace(content, "@TOOLS_RELEASE@", rpmRelease, -1)
		content = strings.Replace(content, "@ARCHITECTURE@", pf.Arch, -1)
		_, err = f.WriteString(content)
		check(err, "write content to spec file")
	}
	createSpecFile()

	outputFile := mdt + "-" + rpmVersion + "-" + rpmRelease + "." + pf.Arch + ".rpm"
	outputPath := filepath.Join(home, "rpmbuild", "RPMS", outputFile)
	// create the .deb file.
	log.Printf("running: rpmbuild -bb %s\n", specFile)
	out, err := run("rpmbuild", "-bb", specFile)
	check(err, "rpmbuild\n"+out)
	// Copy to top level directory so we can upload it.
	check(copyFile(
		outputPath,
		filepath.Join("../release.rpm"),
	), "linking output for s3 upload")
}

func buildDeb() {
	pf, err := platform.GetFromEnv()
	check(err, "get platform")

	mdt := "mongodb-database-tools"
	releaseName := getReleaseName()

	// set up build working directory.
	cdBack := useWorkingDir("deb_build")
	// we'll want to go back to the original directory, just in case.
	defer cdBack()

	// The goal here is to set up  directory with the following structure:
	// releaseName/
	// |----- DEBIAN/
	// |        |----- control
	// |        |----- postinst
	// |        |----- prerm
	// |        |----- md5sums
	// |------ usr/
	//          |-- bin/
	//          |    |--- bsondump
	//          |    |--- mongo*
	//          |-- share/
	//                 |---- doc/
	//                        |----- mongodb-database-tools/
	//                                         |--- staticFiles

	log.Printf("create deb directory tree\n")

	// create DEBIAN dir
	controlDir := filepath.Join(releaseName, "DEBIAN")
	check(os.MkdirAll(controlDir, os.ModePerm), "mkdirAll "+controlDir)

	// create usr/bin and usr/share/doc
	binDir := filepath.Join(releaseName, "usr", "bin")
	check(os.MkdirAll(binDir, os.ModePerm), "mkdirAll "+binDir)
	docDir := filepath.Join(releaseName, "usr", "share", "doc", mdt)
	check(os.MkdirAll(docDir, os.ModePerm), "mkdirAll "+docDir)

	md5sums := make(map[string]string)
	// We use the order just to make sure the md5sums are always in the same order.
	// This probably doesn't matter, but it looks nicer for anyone inspecting the md5sums file.
	md5sumsOrder := make([]string, 0, len(binaries)+len(staticFiles))
	logCopy := func(src, dst string) {
		log.Printf("copying %s to %s\n", src, dst)
	}
	// Copy over the data files.
	{
		binariesPath := filepath.Join("..", "bin")
		// Add binaries.
		for _, binName := range binaries {
			src := filepath.Join(binariesPath, binName)
			dst := filepath.Join(binDir, binName)
			logCopy(src, dst)
			check(os.Link(src, dst), "link file")
			md5sums[dst] = computeMD5(src)
			md5sumsOrder = append(md5sumsOrder, dst)
		}
		// Add static files.
		for _, file := range staticFiles {
			src := filepath.Join("..", file)
			dst := filepath.Join(docDir, file)
			logCopy(src, dst)
			check(os.Link(src, dst), "link file")
			md5sums[dst] = computeMD5(src)
			md5sumsOrder = append(md5sumsOrder, dst)
		}
	}

	controlFile := "control"
	createControlFile := func() {
		f, err := os.Create(controlFile)
		check(err, "create control")
		defer f.Close()

		v, err := version.GetCurrent()
		check(err, "get version")

		// get the control file content.
		contentBytes, err := ioutil.ReadFile(filepath.Join("..", "installer", "deb", "control"))
		content := string(contentBytes)
		check(err, "reading control file content")
		content = strings.Replace(content, "@TOOLS_VERSION@", v.String(), -1)

		content = strings.Replace(content, "@ARCHITECTURE@", pf.DebianArch(), 1)
		_, err = f.WriteString(content)
		check(err, "write content to control file")
	}
	createControlFile()

	md5sumsFile := "md5sums"
	createMD5Sums := func() {
		f, err := os.Create(md5sumsFile)
		check(err, "create md5sums")
		defer f.Close()
		os.Chmod(md5sumsFile, 0644)
		// create the md5sums file.
		for _, path := range md5sumsOrder {
			md5sum, ok := md5sums[path]
			if !ok {
				log.Fatalf("could not find md5sum for " + path)
			}
			_, err = f.WriteString(md5sum + " ")
			check(err, "write md5sum to md5sums")
			_, err = f.WriteString(path + "\n")
			check(err, "write path to md5sums")
		}
	}
	createMD5Sums()

	// Copy the control files to our controlDir
	// control -- metadata
	// md5sums (optional) -- sums for all files
	// postinst (optional) -- post install script, we don't need this
	// prerm (optional) -- removing old documentation
	{
		staticControlFiles := []string{
			"postinst",
			"prerm",
		}
		// add the control file.
		dst := filepath.Join(controlDir, controlFile)
		logCopy(controlFile, dst)
		check(os.Link(controlFile, dst), "link file")

		// add the md5sumsFile.
		dst = filepath.Join(controlDir, md5sumsFile)
		logCopy(md5sumsFile, dst)
		check(os.Link(md5sumsFile, dst), "link file")

		// add the static control files.
		for _, file := range staticControlFiles {
			// add the static control files.
			src := filepath.Join("..", "installer", "deb", file)
			dst = filepath.Join(controlDir, file)
			logCopy(src, dst)
			check(os.Link(src, dst), "link file")
		}
	}

	output := releaseName + ".deb"
	// create the .deb file.
	log.Printf("running: dpkg -D1 -b %s %s", releaseName, output)
	out, err := run("dpkg", "-D1", "-b", releaseName, output)
	check(err, "run dpkg\n"+out)
	// Copy to top level directory so we can upload it.
	check(os.Link(
		output,
		filepath.Join("../release.deb"),
	), "linking output for s3 upload")
}

func buildMSI() {
	pf, err := platform.GetFromEnv()
	check(err, "get platform")
	if pf.OS != platform.OSWindows {
		return
	}

	// The msi msiUpgradeCode must be updated when the minor version changes.
	msiUpgradeCode := "f8a84cb5-a2a7-4392-bfb5-8f829b659960"

	binariesPath := filepath.Join("..", "bin")
	msiStaticFilesPath := ".."
	// Note that the file functions do not allow for drive letters on Windows, absolute paths
	// must be specified with a leading os.PathSeparator.
	saslDLLsPath := string(os.PathSeparator) + filepath.Join("sasl", "bin")
	msiFilesPath := filepath.Join("..", "installer", "msi")

	// These are the meta-text files that are part of mongo-tools, relative
	// to the location of this go file. We have to use an rtf verison of the
	// license, so we do not include the static files.
	var msiStaticFiles = []string{
		"README.md",
		"THIRD-PARTY-NOTICES",
	}

	var saslDLLs = []string{
		"libsasl.dll",
	}

	// location of the necessary data files to build the msi.
	var msiFiles = []string{
		"Banner_Tools.bmp",
		"BinaryFragment.wxs",
		"Dialog.bmp",
		"Dialog_Tools.bmp",
		"FeatureFragment.wxs",
		"Installer_Icon_16x16.ico",
		"Installer_Icon_32x32.ico",
		"LICENSE.rtf",
		"LicensingFragment.wxs",
		"Product.wxs",
		"UIFragment.wxs",
	}

	log.Printf("building msi installer\n")

	// set up build working directory.
	msiBuildDir := "msi_build"
	cdBack := useWorkingDir(msiBuildDir)
	// we'll want to go back to the original directory, just in case.
	defer cdBack()

	// Copy sasldlls. They need to be in this directory for Wix. Linking will
	// not work as the dlls are on a different file system.
	for _, name := range saslDLLs {
		err := copyFile(
			filepath.Join(saslDLLsPath, name),
			name,
		)
		check(err, "copy sasl dlls into "+msiBuildDir)
	}

	// make links to all the staticFiles. They need to be in this
	// directory for Wix.
	for _, name := range msiStaticFiles {
		err := os.Link(
			filepath.Join(msiStaticFilesPath, name),
			name,
		)
		check(err, "link msi static files into "+msiBuildDir)
	}

	for _, name := range msiFiles {
		err := os.Link(
			filepath.Join(msiFilesPath, name),
			name,
		)
		check(err, "link msi creation files into "+msiBuildDir)
	}

	for _, name := range binaries {
		err := os.Link(
			filepath.Join(binariesPath, name),
			name+".exe",
		)
		check(err, "link binary files into "+msiBuildDir)
	}

	// Wix requires the directories to end with a separator.
	cwd, err := os.Getwd()
	check(err, "getwd")
	cwd += "\\"
	wixPath := string(os.PathSeparator) + filepath.Join("wixtools", "bin")
	wixUIExtPath := filepath.Join(wixPath, "WixUIExtension.dll")
	projectName := "MongoDB Tools"
	sourceDir := cwd
	resourceDir := cwd
	binDir := cwd
	objDir := filepath.Join(cwd, "objs") + string(os.PathSeparator)
	arch := "x64"

	v, err := version.GetCurrent()
	check(err, "get version")

	wixVersion := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	versionLabel := fmt.Sprintf("%d", v.Major)

	lastVersionLabel := "100"
	if versionLabel != lastVersionLabel {
		check(fmt.Errorf("msiUpgradeCode in release.go must be updated"), "msiUpgradeCode should be up-to-date, last version = "+lastVersionLabel)
	}

	candle := filepath.Join(wixPath, "candle.exe")
	out, err := run(candle,
		"-wx",
		`-dProductId=*`,
		`-dPlatform=x64`,
		`-dUpgradeCode=`+msiUpgradeCode,
		`-dVersion=`+wixVersion,
		`-dVersionLabel=`+versionLabel,
		`-dProjectName=`+projectName,
		`-dSourceDir=`+sourceDir,
		`-dResourceDir=`+resourceDir,
		`-dSslDir=`+binDir,
		`-dBinaryDir=`+binDir,
		`-dTargetDir=`+objDir,
		`-dTargetExt=".msi"`,
		`-dTargetFileName="release"`,
		`-dOutDir=`+objDir,
		`-dConfiguration="Release"`,
		`-arch`, arch,
		`-out`, objDir,
		`-ext`, wixUIExtPath,
		`Product.wxs`,
		`FeatureFragment.wxs`,
		`BinaryFragment.wxs`,
		`LicensingFragment.wxs`,
		`UIFragment.wxs`,
	)

	check(err, "run candle.exe\n"+out)

	output := "release.msi"
	light := filepath.Join(wixPath, "light.exe")
	out, err = run(light,
		"-wx",
		`-cultures:en-us`,
		`-out`, output,
		`-ext`, wixUIExtPath,
		filepath.Join(objDir, `Product.wixobj`),
		filepath.Join(objDir, `FeatureFragment.wixobj`),
		filepath.Join(objDir, `BinaryFragment.wixobj`),
		filepath.Join(objDir, `LicensingFragment.wixobj`),
		filepath.Join(objDir, `UIFragment.wixobj`),
	)
	check(err, "run light.exe\n"+out)

	// Copy to top level directory so we can upload it.
	check(os.Link(
		output,
		filepath.Join("..", output),
	), "linking output for s3 upload")
}

func copyFile(src, dst string) error {
	file, err := os.Open(src)
	check(err, "open src")
	defer file.Close()

	out, err := os.Create(dst)
	check(err, "create dst")
	defer out.Close()

	_, err = io.Copy(out, file)
	check(err, "copy src -> dst")
	return out.Close()
}

func downloadFile(url, dst string) {
	out, err := os.Create(dst)
	check(err, "create release file")
	defer out.Close()

	resp, err := http.Get(url)
	check(err, "download release file")
	defer resp.Body.Close()

	_, err = io.Copy(out, resp.Body)
	check(err, "write release file from http body")
}

func computeMD5(filename string) string {
	content, err := ioutil.ReadFile(filename)
	check(err, "reading file during md5 summing")
	return fmt.Sprintf("%x", md5.Sum([]byte(content)))
}

func computeSHA1(filename string) string {
	content, err := ioutil.ReadFile(filename)
	check(err, "reading file during sha1 summing")
	return fmt.Sprintf("%x", sha1.Sum([]byte(content)))
}

func computeSHA256(filename string) string {
	content, err := ioutil.ReadFile(filename)
	check(err, "reading file during sha256 summing")
	return fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
}

func useWorkingDir(dir string) func() {
	check(os.RemoveAll(dir), "removeAll "+dir)
	check(os.MkdirAll(dir, os.ModePerm), "mkdirAll "+dir)
	check(os.Chdir(dir), "cd to "+dir)

	cur, err := os.Getwd()
	check(err, "get current directory")
	return func() {
		os.Chdir(cur)
	}
}

func addToTarball(tw *tar.Writer, dst, src string) {
	file, err := os.Open(src)
	check(err, "open file")
	defer file.Close()

	stat, err := file.Stat()
	check(err, "stat file")

	header := &tar.Header{
		Name: dst,
		Size: stat.Size(),
		Mode: int64(stat.Mode()),
	}

	err = tw.WriteHeader(header)
	check(err, "write header to archive")

	_, err = io.Copy(tw, file)
	check(err, "write file to archive")
}

func buildTarball() {
	log.Printf("building tarball archive\n")

	archiveFile, err := os.Create("release.tgz")
	check(err, "create archive file")
	defer archiveFile.Close()

	gw := gzip.NewWriter(archiveFile)
	defer gw.Close()

	tw := tar.NewWriter(gw)
	defer tw.Close()

	releaseName := getReleaseName()

	for _, name := range staticFiles {
		log.Printf("adding %s to tarball\n", name)
		src := name
		dst := filepath.Join(releaseName, name)
		addToTarball(tw, dst, src)
	}

	for _, binName := range binaries {
		log.Printf("adding %s binary to tarball\n", binName)
		src := filepath.Join("bin", binName)
		dst := filepath.Join(releaseName, "bin", binName)
		addToTarball(tw, dst, src)
	}
}

func addToZip(zw *zip.Writer, dst, src string) {
	file, err := os.Open(src)
	check(err, "open file")
	defer file.Close()

	stat, err := file.Stat()
	check(err, "stat file")

	header, err := zip.FileInfoHeader(stat)
	check(err, "construct zip header from stat")
	header.Name = dst
	header.Method = 8

	fw, err := zw.CreateHeader(header)
	check(err, "create header")

	_, err = io.Copy(fw, file)
	check(err, "write file to zip")
}

func buildZip() {
	log.Printf("building zip archive\n")

	archiveFile, err := os.Create("release.zip")
	check(err, "create archive file")
	defer archiveFile.Close()

	zw := zip.NewWriter(archiveFile)
	defer zw.Close()

	releaseName := getReleaseName()

	for _, name := range staticFiles {
		log.Printf("adding %s to zip\n", name)
		src := name
		dst := filepath.Join(releaseName, name)
		addToZip(zw, dst, src)
	}

	for _, binName := range binaries {
		log.Printf("adding %s binary to zip\n", binName)
		src := filepath.Join(".", "bin", binName)
		dst := filepath.Join(releaseName, "bin", binName+".exe")
		addToZip(zw, dst, src)
	}
}

func uploadRelease(v version.Version) {
	if env.EvgIsPatch() {
		fmt.Println("current build is a patch; not uploading a release")
	}

	tasks, err := evergreen.GetTasksForRevision(v.Commit)
	check(err, "get evergreen tasks")

	signTasks := []evergreen.Task{}
	for _, task := range tasks {
		if task.IsPatch() || task.DisplayName != "sign" {
			continue
		}

		_, knownVariant := platform.GetByVariant(task.Variant)
		if !knownVariant {
			log.Fatalf("found sign task with unknown variant '%s'\n", task.Variant)
		}

		signTasks = append(signTasks, task)
	}

	if len(signTasks) != platform.Count() {
		log.Fatalf(
			"found %d sign tasks, but expected %d release platforms",
			len(signTasks), platform.Count(),
		)
	}

	awsClient, err := aws.GetClient()
	check(err, "get aws client")

	// Accumulate all downloaded artifacts from sign tasks for JSON feed.
	var dls []download.ToolsDownload

	for _, task := range signTasks {
		fmt.Printf("\ngetting artifacts for %s\n", task.Variant)
		pf, ok := platform.GetByVariant(task.Variant)
		if !ok {
			panic("unreachable") // should have been caught in previous block
		}

		artifacts, err := evergreen.GetArtifactsForTask(task.TaskID)
		check(err, "getting artifacts list")

		if len(artifacts) != len(pf.ArtifactExtensions()) {
			log.Fatalf(
				"expected %d artifacts but found %d for %s",
				len(pf.ArtifactExtensions()), len(artifacts), task.Variant,
			)
		}

		var dl download.ToolsDownload
		dl.Name = pf.Name
		dl.Arch = pf.Arch
		for _, a := range artifacts {
			ext := path.Ext(a.URL)

			unstableFile := fmt.Sprintf(
				"mongodb-database-tools-%s-%s-unstable%s",
				pf.Name, pf.Arch, ext,
			)

			stableFile := fmt.Sprintf(
				"mongodb-database-tools-%s-%s-%s%s",
				pf.Name, pf.Arch, v, ext,
			)

			latestStableFile := fmt.Sprintf(
				"mongodb-database-tools-%s-%s-latest-stable%s",
				pf.Name, pf.Arch, ext,
			)

			fmt.Printf("  downloading %s\n", a.URL)
			downloadFile(a.URL, unstableFile)
			if v.IsStable() {
				copyFile(unstableFile, stableFile)
				copyFile(unstableFile, latestStableFile)

				// The artifact URL indicates whether the artifact is an archive or a package.
				// We assume there's at most one archive artifact and one package artifact
				// for a given download entry.
				artifactURL := fmt.Sprintf("https://fastdl.mongodb.org/tools/db/%s", stableFile)
				md5sum := computeMD5(latestStableFile)
				sha1sum := computeSHA1(latestStableFile)
				sha256sum := computeSHA256(latestStableFile)

				if ext == ".tgz" || ext == ".zip" {
					dl.Archive = download.ToolsArchive{URL: artifactURL, Md5: md5sum, Sha1: sha1sum, Sha256: sha256sum}
				} else {
					dl.Package = &download.ToolsPackage{URL: artifactURL, Md5: md5sum, Sha1: sha1sum, Sha256: sha256sum}
				}
			}

			fmt.Printf("    uploading to https://s3.amazonaws.com/downloads.mongodb.org/tools/db/%s\n", unstableFile)
			awsClient.UploadFile("downloads.mongodb.org", "/tools/db", unstableFile)
			if v.IsStable() {
				fmt.Printf("    uploading to https://s3.amazonaws.com/downloads.mongodb.org/tools/db/%s\n", stableFile)
				awsClient.UploadFile("downloads.mongodb.org", "/tools/db", stableFile)
				fmt.Printf("    uploading to https://s3.amazonaws.com/downloads.mongodb.org/tools/db/%s\n", latestStableFile)
				awsClient.UploadFile("downloads.mongodb.org", "/tools/db", latestStableFile)
			}
		}

		dls = append(dls, dl)
	}

	// We only have one version for now, so we can just append one ToolsVersion to the JSON
	// feed and upload immediately. Supporting more versions will require an additional loop.
	if v.IsStable() {
		var feed download.JSONFeed
		feed.Versions = append(feed.Versions, download.ToolsVersion{Version: v.StringWithoutPre(), Downloads: dls})

		feedFilename := "release.json"
		feedFile, err := os.Create(feedFilename)
		check(err, "create release.json")
		defer feedFile.Close()

		jsonEncoder := json.NewEncoder(feedFile)
		jsonEncoder.SetIndent("", "  ")
		err = jsonEncoder.Encode(feed)
		check(err, "encode json feed")

		fmt.Printf("uploading download feed to https://s3.amazonaws.com/downloads.mongodb.org/tools/db/%s\n", feedFilename)
		awsClient.UploadFile("downloads.mongodb.org", "/tools/db", feedFilename)
	}
}
