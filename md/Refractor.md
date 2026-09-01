You're right to ask for clarification on the abstraction levels in Clean Architecture and Domain-Driven Design (DDD)! It's a common point of confusion.

In essence, the goal is to separate concerns and create layers of abstraction so that the core business logic (the Domain) is independent of external concerns like databases, UIs, or specific frameworks.

Let's break down where your current code elements fit and where they should ideally go.

---

### 🏛️ Clean Architecture / DDD Layering & Your Code

Here's a guide on how to map your existing code components to the layers and the logic that belongs in each:

1.  **Domain Layer (Core Business Logic):**
    *   **What it contains:** Pure business entities, value objects, domain events, and the abstract interfaces for repositories (e.g., `ProjectRepository` you defined earlier). This layer should have *no dependencies* on anything outside itself, especially not frameworks, databases, or external services.
    *   **Your code here:**
        *   `entity.Project` (and its status constants/methods)
        *   `entity.Settings`
        *   `entity.ProjectWithSettings`
        *   The `ProjectRepository` interface definition (though sometimes this is placed in the Application layer if it defines data access *for* use cases). For true DDD, repository *interfaces* are often defined in the Domain layer.

2.  **Application Layer (Use Cases):**
    *   **What it contains:** Orchestrates the flow of data to and from the domain layer. It contains the "use cases" or "application business rules." These use cases orchestrate calls to domain entities and domain services, and they use repository *interfaces* to get/save data. They know *how* to perform specific application tasks.
    *   **Your code here (This is where your `ProjectService` *should* largely go, but refactored):**
        *   The logic currently in `service.ProjectService.StartNodeApplication` should be refactored into a use case like `StartProjectUseCase`.
        *   You'd need a new use case for handling uploads, perhaps `UploadProjectUseCase`.
        *   A use case for retrieving project details (`GetProjectUseCase`).
        *   A use case for managing settings (`UpdateProjectSettingsUseCase`, `GetProjectSettingsUseCase`).
        *   Crucially, these use cases will depend on the *interfaces* defined in the Domain layer (e.g., `ProjectRepository`) and potentially other application services (like a "subprocess runner" or "port allocator" service, which are *implementation details* of the application layer).
        *   **Important:** The use case itself should *not* know *how* to run `npm` or manage `exec.Cmd`. It should delegate that to an infrastructure component.

3.  **Infrastructure Layer (Technical Details):**
    *   **What it contains:** Concrete implementations of interfaces defined in the Domain/Application layers. This is where databases, web frameworks, external API clients, file system operations, command execution, etc., live. It's the "outermost" layer that "plugs into" the inner layers.
    *   **Your code here:**
        *   **`utils` package:** Most of this belongs here.
            *   `utils.FileExists`, `filepath.Join` (standard library, but used here)
            *   `utils.NewID` (an ID generation utility)
            *   `utils.SaveZipFile`, `utils.Unzip` (file system operations)
            *   `utils.PickFreePort` (utility for finding available resources)
        *   **`service.ProjectService.StartNodeApplication`'s *implementation details*:**
            *   The direct calls to `exec.Command("npm", ...)` and `cmd.Run()`, `cmd.Start()`, `cmd.Wait()`. This specific logic of *how* to execute external processes belongs here. You might create a `SubprocessRunner` struct or similar in the infrastructure layer that implements an interface defined in the Application layer (e.g., `ProcessRunner`).
        *   **HTTP Handlers (`UploadHandler`, `RunHandler`):** These are part of the "Interface Adapters" or "Presentation Layer" which is often considered part of or closely related to the infrastructure. They translate requests from the web into calls to the Application Layer (Use Cases) and then translate the results back into HTTP responses.
            *   `UploadHandler`: Should call `UploadProjectUseCase`. It handles HTTP specifics like `multipart/form-data`, file saving (delegating to infrastructure utils), and error responses.
            *   `RunHandler`: Should call `StartProjectUseCase`. It handles HTTP specifics like picking a port (delegating to infrastructure utils), managing the `running` map (this map management might be better suited to the application layer or a dedicated runtime manager, not directly in the handler).
        *   **Database Implementation:** If you were implementing `ProjectRepository`, the code interacting with PostgreSQL (SQL queries, `sql.DB` usage) would live here.
        *   **Web Server:** The `http.ListenAndServe` call and routing setup would be in the `cmd/runner/main.go` entry point.

---

### Refactoring Summary & Where Logic Goes:

*   **`entity.Project`, `entity.Settings`, `ProjectStatus`:** Domain Layer (Entities).
*   **`repository.ProjectRepository` (interface):** Domain Layer (or Application Layer if defining data access needs *for* use cases).
*   **`service.ProjectService.StartNodeApplication` (the *what* and *orchestration*):** Application Layer (Use Cases). E.g., `StartProjectUseCase`.
*   **`service.ProjectService.StartNodeApplication` (the *how* - `exec.Command`, `npm install`, `npm start`):** Infrastructure Layer (e.g., `SubprocessExecutor` implementation).
*   **`utils` package (file ops, ID gen, port picking):** Infrastructure Layer.
*   **`UploadHandler`, `RunHandler`:** Infrastructure Layer / Presentation Layer / Interface Adapters. They act as gateways, calling Use Cases.
*   **`running` map & `runningMu`:** This is tricky. Managing active processes often falls to the Application Layer (a specific "Runtime Manager" or "Deployment Manager" use case/service) or even Infrastructure if it's tied to specific runtime environments. Handlers shouldn't typically manage the lifecycle of other processes directly.

Think of it this way:

*   **Domain:** *What* the business is about (Projects, Users, their properties and rules).
*   **Application:** *What actions* the system can perform (Upload a project, Start a project, Get settings). It orchestrates domain objects.
*   **Infrastructure:** *How* those actions are technically performed (Execute a command, Save to DB, Listen on HTTP, Write to disk).

Your current `ProjectService` is a mix of Application and Infrastructure logic. The handlers are also mixed, doing HTTP handling *and* directly calling infrastructure utilities and your mixed service. The refactoring aims to separate these concerns cleanly.