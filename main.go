package main

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"strings"
	"time"

	cdx "github.com/CycloneDX/cyclonedx-go"
	"github.com/google/uuid"

	"github.com/CycloneDX/cyclonedx-gomod/internal/gomod"
	"github.com/CycloneDX/cyclonedx-gomod/internal/version"
)

var (
	componentType  string
	modulePath     string
	noSerialNumber bool
	outputPath     string
	serialNumber   string
	showVersion    bool
	useJSON        bool

	allowedComponentTypes = []cdx.ComponentType{
		cdx.ComponentTypeApplication,
		cdx.ComponentTypeContainer,
		cdx.ComponentTypeDevice,
		cdx.ComponentTypeFile,
		cdx.ComponentTypeFirmware,
		cdx.ComponentTypeFramework,
		cdx.ComponentTypeLibrary,
		cdx.ComponentTypeOS,
	}

	// See https://labix.org/gopkg.in
	// gopkg.in/pkg.v3		→ github.com/go-pkg/pkg	(branch/tag v3, v3.N, or v3.N.M)
	// gopkg.in/user/pkg.v3	→ github.com/user/pkg  	(branch/tag v3, v3.N, or v3.N.M)
	goPkgInRegex1 = regexp.MustCompile("^gopkg\\.in/([^/]+)/([^.]+)\\..*$") // With user segment
	goPkgInRegex2 = regexp.MustCompile("^gopkg\\.in/([^.]+)\\..*$")         // Without user segment
)

func main() {
	flag.StringVar(&componentType, "type", string(cdx.ComponentTypeApplication), "Type of the main component")
	flag.StringVar(&modulePath, "module", ".", "Path to Go module")
	flag.BoolVar(&noSerialNumber, "noserial", false, "Omit serial number")
	flag.StringVar(&outputPath, "output", "-", "Output path")
	flag.StringVar(&serialNumber, "serial", "", "Serial number (default [random UUID])")
	flag.BoolVar(&showVersion, "version", false, "Show version")
	flag.BoolVar(&useJSON, "json", false, "Output in JSON format")
	flag.Parse()

	if showVersion {
		fmt.Println(version.Version)
		return
	}

	if err := validateArguments(); err != nil {
		log.Fatal(err)
	}

	modules, err := gomod.GetModules(modulePath)
	if err != nil {
		log.Fatalf("failed to get modules: %v", err)
	}

	mainModule := modules[0]
	modules = modules[1:]

	// Detect main module version
	if tagVersion, err := gomod.GetVersionFromTag(mainModule.Dir); err != nil {
		pseudoVersion, err := gomod.GetPseudoVersion(mainModule.Dir)
		if err != nil {
			log.Fatalf("failed to detect version of main module: %v", err)
		}
		mainModule.Version = pseudoVersion
	} else {
		mainModule.Version = tagVersion
	}

	// Normalize versions
	for i := range modules {
		modules[i].Version = strings.TrimSuffix(modules[i].Version, "+incompatible")
	}

	bom := cdx.NewBOM()
	if !noSerialNumber {
		if serialNumber == "" {
			bom.SerialNumber = uuid.New().URN()
		} else {
			bom.SerialNumber = "urn:uuid:" + serialNumber
		}
	}

	toolHashes, err := calculateToolHashes()
	if err != nil {
		log.Fatalf("failed to calculate tool hashes: %v", err)
	}

	mainComponent := convertToComponent(mainModule)
	mainComponent.Scope = "" // Main component can't have a scope
	mainComponent.Type = cdx.ComponentType(componentType)

	bom.Metadata = &cdx.Metadata{
		Timestamp: time.Now().Format(time.RFC3339),
		Tools: &[]cdx.Tool{
			{
				Vendor:  version.Author,
				Name:    version.Name,
				Version: version.Version,
				Hashes:  &toolHashes,
			},
		},
		Component: &mainComponent,
	}

	components := make([]cdx.Component, len(modules))
	for i, module := range modules {
		components[i] = convertToComponent(module)
	}
	bom.Components = &components

	dependencyGraph := buildDependencyGraph(append(modules, mainModule))
	bom.Dependencies = &dependencyGraph

	var outputFormat cdx.BOMFileFormat
	if useJSON {
		outputFormat = cdx.BOMFileFormatJSON
	} else {
		outputFormat = cdx.BOMFileFormatXML
	}

	var outputWriter io.Writer
	if outputPath == "" || outputPath == "-" {
		outputWriter = os.Stdout
	} else {
		outputFile, err := os.Create(outputPath)
		if err != nil {
			log.Fatalf("failed to create output file %s: %v", outputPath, err)
		}
		defer outputFile.Close()
		outputWriter = outputFile
	}

	encoder := cdx.NewBOMEncoder(outputWriter, outputFormat)
	encoder.SetPretty(true)

	if err = encoder.Encode(bom); err != nil {
		log.Fatalf("encoding BOM failed: %v", err)
	}
}

func validateArguments() error {
	isAllowedComponentType := false
	for i := range allowedComponentTypes {
		if allowedComponentTypes[i] == cdx.ComponentType(componentType) {
			isAllowedComponentType = true
			break
		}
	}
	if !isAllowedComponentType {
		return fmt.Errorf("invalid component type %s. See https://pkg.go.dev/github.com/CycloneDX/cyclonedx-go#ComponentType for options", componentType)
	}

	// Serial numbers must be valid UUIDs
	if !noSerialNumber && serialNumber != "" {
		if _, err := uuid.Parse(serialNumber); err != nil {
			return fmt.Errorf("invalid serial number: %w", err)
		}
	}

	return nil
}

func calculateToolHashes() ([]cdx.Hash, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}

	exeFile, err := os.Open(exe)
	if err != nil {
		return nil, err
	}
	defer exeFile.Close()

	hashMD5 := md5.New()
	hashSHA1 := sha1.New()
	hashSHA256 := sha256.New()
	hashSHA512 := sha512.New()
	hashWriter := io.MultiWriter(hashMD5, hashSHA1, hashSHA256, hashSHA512)

	if _, err = io.Copy(hashWriter, exeFile); err != nil {
		return nil, err
	}

	return []cdx.Hash{
		{Algorithm: cdx.HashAlgoMD5, Value: fmt.Sprintf("%x", hashMD5.Sum(nil))},
		{Algorithm: cdx.HashAlgoSHA1, Value: fmt.Sprintf("%x", hashSHA1.Sum(nil))},
		{Algorithm: cdx.HashAlgoSHA256, Value: fmt.Sprintf("%x", hashSHA256.Sum(nil))},
		{Algorithm: cdx.HashAlgoSHA512, Value: fmt.Sprintf("%x", hashSHA512.Sum(nil))},
	}, nil
}

func convertToComponent(module gomod.Module) cdx.Component {
	if module.Replace != nil {
		replacementComponent := convertToComponent(*module.Replace)

		module.Replace = nil // Avoid endless recursion
		replacedComponent := convertToComponent(module)
		replacedComponent.Scope = ""

		replacementComponent.Pedigree = &cdx.Pedigree{
			Ancestors: &[]cdx.Component{replacedComponent},
		}

		return replacementComponent
	}

	component := cdx.Component{
		BOMRef:     module.PackageURL(),
		Type:       cdx.ComponentTypeLibrary,
		Name:       module.Path,
		Version:    module.Version, // TODO: Make it configurable to strip the "v" prefix?
		Scope:      cdx.ScopeRequired,
		PackageURL: module.PackageURL(),
	}

	// We currently don't have an accurate way of hashing the main module, as it may contain
	// files that are .gitignore'd and thus not part of the hashes in Go's sumdb.
	// Maybe we need to copy and modify the code from https://github.com/golang/mod/blob/release-branch.go1.15/sumdb/dirhash/hash.go
	if !module.Main {
		hashes, err := calculateModuleHashes(module)
		if err != nil {
			log.Fatal(err)
		}
		component.Hashes = &hashes
	}

	if vcsURL := resolveVcsURL(module); vcsURL != "" {
		component.ExternalReferences = &[]cdx.ExternalReference{
			{Type: cdx.ERTypeVCS, URL: vcsURL},
		}
	}

	return component
}

func calculateModuleHashes(module gomod.Module) ([]cdx.Hash, error) {
	h1, err := module.Hash()
	if err != nil {
		return nil, fmt.Errorf("failed to calculate h1 hash for %s: %w", module.Coordinates(), err)
	}

	h1Bytes, err := base64.StdEncoding.DecodeString(h1[3:])
	if err != nil {
		return nil, fmt.Errorf("failed to base64 decode h1 hash: %w", err)
	}

	return []cdx.Hash{
		{Algorithm: cdx.HashAlgoSHA256, Value: fmt.Sprintf("%x", h1Bytes)},
	}, nil
}

func resolveVcsURL(module gomod.Module) string {
	if strings.Index(module.Path, "github.com/") == 0 {
		return "https://" + module.Path
	} else if goPkgInRegex1.MatchString(module.Path) {
		return "https://" + goPkgInRegex1.ReplaceAllString(module.Path, "github.com/$1/$2")
	} else if goPkgInRegex2.MatchString(module.Path) {
		return "https://" + goPkgInRegex2.ReplaceAllString(module.Path, "github.com/go-$1/$1")
	}
	return ""
}

func buildDependencyGraph(modules []gomod.Module) []cdx.Dependency {
	depGraph := make([]cdx.Dependency, 0)

	for _, module := range modules {
		cdxDependant := cdx.Dependency{Ref: module.PackageURL()}
		if module.Dependencies != nil {
			cdxDependencies := make([]cdx.Dependency, len(module.Dependencies))
			for i := range module.Dependencies {
				cdxDependencies[i] = cdx.Dependency{Ref: module.Dependencies[i].PackageURL()}
			}
			if len(cdxDependencies) > 0 {
				cdxDependant.Dependencies = &cdxDependencies
			}
		}
		depGraph = append(depGraph, cdxDependant)
	}

	return depGraph
}
