// pathbind-gen generates consumer-facing code from pathbind configuration files.
//
// Modes:
//
//	--mode=init   reads field_metadata.json and writes pathbind-draft.yaml.
//	              Run by the SDK's make generate-pathbind-draft target.
//
//	--mode=cobra  reads pathbind-draft.yaml + pathbind-overrides.yaml and writes
//	              Go source (input struct, RegisterXxxFlags, XxxHandler interface,
//	              GeneratedXxxPrompt, RunXxx) into the consumer's package.
//	              Run by the consumer's make generate-seaplane target.
//
//	--mode=tf     reads pathbind-draft.yaml + pathbind-overrides.yaml and writes
//	              Go source (input struct, handler interface, resource scaffolding)
//	              into the consumer's package.
//	              Run by the consumer's make generate-seaplane target.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/openshift-online/rosa-seaplane-api/clientset/cmd/pathbind-gen/cobra"
	"github.com/openshift-online/rosa-seaplane-api/clientset/cmd/pathbind-gen/tf"
)

func main() {
	mode := flag.String("mode", "", "generation mode: init, cobra, or tf (required)")

	// init mode
	registryPath := flag.String("registry", "", "[init] path to field_metadata.json")
	openapiPath := flag.String("openapi", "", "[init] path to openapi.yaml for leaf-path resolution (optional)")
	draftOutput := flag.String("output", "", "[init] path to write pathbind-draft.yaml")

	// cobra mode
	draftPath := flag.String("draft", "", "[cobra/tf] path to pathbind-draft.yaml (optional)")
	overridesPath := flag.String("overrides", "", "[cobra/tf] path to pathbind-overrides.yaml")
	outputDir := flag.String("output-dir", "", "[cobra/tf] directory to write generated Go files")
	headerFile := flag.String("go-header-file", "", "[cobra] path to license header file (defaults to the repository boilerplate)")

	flag.Parse()

	if *mode == "" {
		fatalf("--mode is required; use init, cobra, or tf")
	}

	switch *mode {
	case "init":
		if *registryPath == "" || *draftOutput == "" {
			fatalf("--registry and --output are required for --mode=init")
		}
		if err := runInit(*registryPath, *openapiPath, *draftOutput); err != nil {
			log.Fatalf("pathbind-gen: %v", err)
		}

	case "cobra":
		if *overridesPath == "" || *outputDir == "" {
			fatalf("--overrides and --output-dir are required for --mode=cobra")
		}
		if err := cobra.Run(*draftPath, *overridesPath, *outputDir, *headerFile); err != nil {
			log.Fatalf("pathbind-gen: %v", err)
		}

	case "tf":
		if *draftPath == "" || *overridesPath == "" || *outputDir == "" {
			fatalf("--draft, --overrides, and --output-dir are required for --mode=tf")
		}
		if err := runTF(*draftPath, *overridesPath, *outputDir); err != nil {
			log.Fatalf("pathbind-gen: %v", err)
		}

	default:
		fatalf("unknown mode %q; use init, cobra, or tf", *mode)
	}
}

func runTF(draftPath, overridesPath, outputDir string) error {
	return tf.Run(draftPath, overridesPath, outputDir)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "pathbind-gen: "+format+"\n", args...)
	os.Exit(1)
}
