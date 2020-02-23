package container_test

import (
	"log"
	"reflect"
	"testing"

	"github.com/mergermarket/cdflow2/release/container"
	"github.com/mergermarket/cdflow2/test"
)

func TestRelese(t *testing.T) {
	// Given
	dockerClient := test.GetDockerClient()

	outputCollector := test.NewOutputCollector()

	buildVolume := test.CreateVolume(dockerClient)
	defer test.RemoveVolume(dockerClient, buildVolume)

	// When
	releaseMetadata, err := container.Run(
		dockerClient,
		test.GetConfig("TEST_RELEASE_IMAGE"),
		test.GetConfig("TEST_ROOT")+"/test/release/sample-code",
		buildVolume,
		outputCollector.OutputWriter,
		outputCollector.ErrorWriter,
		map[string]string{
			"VERSION":      "test-version",
			"TEAM":         "test-team",
			"COMPONENT":    "test_component",
			"COMMIT":       "test-commit",
			"TEST_VERSION": "test-version",
		},
	)
	if err != nil {
		log.Panicln("unexpected error: ", err)
	}

	// Then
	_, errors, err := outputCollector.Collect()
	if err != nil {
		log.Panicln("error collecting output:", err)
	}

	if errors != "message to stderr from release\ndocker status: OK\n" {
		log.Panicf("unexpected stderr output: '%v'", errors)
	}

	if !reflect.DeepEqual(releaseMetadata, map[string]string{
		"release_var_from_env":    "release value from env",
		"version_from_defaults":   "test-version",
		"team_from_defaults":      "test-team",
		"component_from_defaults": "test_component",
		"commit_from_defaults":    "test-commit",
		"test_from_config":        "test-version",
	}) {
		log.Panicf("unexpected release metadata: %v\n", releaseMetadata)
	}
}
