package api

//go:generate go run ../../cmd/specfix -compat-output openapi.compat.json ../../../apps/web/openapi.json
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.7.2 --config oapi-codegen.yaml openapi.compat.json
