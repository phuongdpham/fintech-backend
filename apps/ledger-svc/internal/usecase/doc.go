// Package usecase contains the application's orchestration layer.
//
// Usecases compose domain operations and adapter ports. They:
//   - validate user input,
//   - drive cross-cutting concerns (idempotency, observability),
//   - call repository methods (which themselves own DB transactions),
//   - and translate adapter errors back into domain errors when needed.
//
// Usecases MUST NOT touch driver-specific types. They depend only on
// domain.* port interfaces, which keeps every usecase trivially testable
// with in-memory fakes.
package usecase
