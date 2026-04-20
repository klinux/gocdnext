// Starter pipeline templates for the "Template" tab of the New
// Project dialog. Each one is a valid .gocdnext/pipeline.yml that
// the backend parser accepts out of the box — no further tweaks
// required — so a user can click "Create" and immediately see a
// working pipeline skeleton they can evolve.

export type PipelineTemplate = {
  id: string;
  label: string;
  description: string;
  filename: string;
  yaml: string;
};

const MINIMAL = `# Minimal pipeline: one stage, one job. Change 'script' to
# something more useful (make, pnpm test, …) once the agent
# runs this cleanly.
pipelines:
  - name: ci
    stages:
      - build
    jobs:
      - name: hello
        stage: build
        tasks:
          - script: echo "hello from gocdnext"
`;

const GO_CI = `# Go CI skeleton: test + build + (optional) docker image.
# Adjust the image version + module path to match your repo.
pipelines:
  - name: ci
    stages:
      - lint
      - test
      - build
    jobs:
      - name: vet
        stage: lint
        image: golang:1.23
        tasks:
          - script: go vet ./...
      - name: unit
        stage: test
        image: golang:1.23
        needs: [vet]
        tasks:
          - script: go test -race ./...
      - name: compile
        stage: build
        image: golang:1.23
        needs: [unit]
        tasks:
          - script: go build ./...
`;

const DOCKER_BUILD = `# Docker build + push. Expects a project secret named
# REGISTRY_TOKEN at \${REGISTRY_TOKEN} and a Dockerfile at repo root.
pipelines:
  - name: image
    stages:
      - build
      - publish
    jobs:
      - name: build
        stage: build
        image: docker:24-cli
        tasks:
          - script: docker build -t "\${IMAGE_REF}" .
      - name: push
        stage: publish
        image: docker:24-cli
        needs: [build]
        tasks:
          - script: echo "\${REGISTRY_TOKEN}" | docker login -u ci --password-stdin
          - script: docker push "\${IMAGE_REF}"
`;

export const pipelineTemplates: PipelineTemplate[] = [
  {
    id: "minimal",
    label: "Minimal — single job",
    description: "One stage, one job running 'echo hello'. Good for smoke-testing a fresh agent.",
    filename: "pipeline.yml",
    yaml: MINIMAL,
  },
  {
    id: "go-ci",
    label: "Go — vet · test · build",
    description: "Classic Go CI skeleton on golang:1.23 with lint/test/build stages.",
    filename: "pipeline.yml",
    yaml: GO_CI,
  },
  {
    id: "docker",
    label: "Docker — build & push",
    description: "Builds the Dockerfile at repo root and pushes via a REGISTRY_TOKEN secret.",
    filename: "pipeline.yml",
    yaml: DOCKER_BUILD,
  },
];

export function findTemplate(id: string): PipelineTemplate | undefined {
  return pipelineTemplates.find((t) => t.id === id);
}
