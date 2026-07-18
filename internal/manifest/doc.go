// Package manifest decodes arenactl fleet definitions: flat YAML documents
// whose vocabulary mirrors the ECS task definition (containerDefinitions,
// portMappings, healthCheck), the ECS service definition (desiredCount) and
// Application Auto Scaling (minCapacity/maxCapacity), so existing ECS JSON
// converts with almost no renaming. Decoding expands ${VAR} environment
// references, attaches the arenactl management marker, and converts to the
// API types (arena.v1.FleetSpec); Encode is the reverse for `arenactl get`.
// Normalization and diffing stay server-side (ApplyFleet).
package manifest
