This is a fantastic leap forward! Adding a database and structuring your project with Clean Architecture will be foundational for scaling. Let's break down the plan:

**Your Goals:**

1.  **Database Integration:** Set up a database for project settings and related data.
2.  **Fastest Possible DB Performance:** Optimize tables and indexing for high concurrency (100k users, 10k projects).
3.  **Clean Architecture:** Refactor the application to follow Clean Architecture principles, defining domain code.
4.  **New Endpoints:** Implement `/upload`, `/settings/:id`, and `/run/:id`.
5.  **Redis for Caching:** (Future) Integrate Redis for speed.

---

## 🚀 Phase 1: Database & Core Tables

For speed and scalability, especially with your target numbers, a robust and performant database is key. Given the need for speed and flexibility, **PostgreSQL** is an excellent choice. It's powerful, reliable, supports advanced indexing, and has great tools for performance tuning.

If you're truly after "fastest ever" and "partial tables," **TimescaleDB** (a PostgreSQL extension) or a dedicated time-series database might be even more performant for specific types of data, but for general project settings and metadata, standard PostgreSQL with proper indexing will be incredibly fast and more versatile. Let's stick with PostgreSQL for now for its versatility.

### 1.1 Database Choice: PostgreSQL

**Why PostgreSQL?**

*   **Advanced Indexing:** Supports B-tree, Hash, GiST, SP-GiST, GIN, and BRIN indexes, which are crucial for optimizing various query types.
*   **JSONB Support:** Can store and query JSON data efficiently, useful for flexible settings.
*   **Reliability & ACID Compliance:** Ensures data integrity.
-   **Extensibility:** Can add extensions like TimescaleDB if needed later.

### 1.2 Database Setup (Conceptual - you'll do this outside Go)

You'd typically use Docker or install PostgreSQL directly. For this conversation, we'll assume you have a running PostgreSQL instance accessible by your Go application.

### 1.3 Core Tables

Let's define the essential tables. We'll focus on clarity and performance.

---

#### 📦 `projects` Table

This table will store information about each deployed project.

| Column Name     | Data Type                 | Constraints/Notes                                    | Performance Notes                                                                         |
| :-------------- | :------------------------ | :--------------------------------------------------- | :---------------------------------------------------------------------------------------- |
| `id`            | `BIGSERIAL`               | PRIMARY KEY                                          | Auto-incrementing, efficient for joins.                                                   |
| `user_id`       | `BIGINT`                  | NOT NULL, FOREIGN KEY (users.id)                     | **Index heavily:** For filtering projects by user.                                        |
| `name`          | `VARCHAR(255)`            | NOT NULL                                             | Project name. Consider uniqueness per user.                                               |
| `unique_key`    | `VARCHAR(64)`             | UNIQUE, NOT NULL                                     | A short, unique identifier for URLs (e.g., `/run/:unique_key`). More user-friendly than ID. |
| `source_type`   | `VARCHAR(50)`             | NOT NULL                                             | e.g., 'zip', 'git_repo' (for future expansion)                                            |
| `source_location`| `VARCHAR(512)`            | NOT NULL                                             | Path to uploaded zip, or Git URL.                                                         |
| `status`        | `VARCHAR(50)`             | NOT NULL DEFAULT 'pending'                           | e.g., 'pending', 'building', 'running', 'failed', 'stopped'                               |
| `port`          | `INTEGER`                 | NULLABLE                                             | The port the project is running on. NULL if not running.                                  |
| `created_at`    | `TIMESTAMP WITH TIME ZONE`| NOT NULL DEFAULT NOW()                               | Useful for auditing.                                                                      |
| `updated_at`    | `TIMESTAMP WITH TIME ZONE`| NOT NULL DEFAULT NOW()                               | Useful for auditing.                                                                      |
| `deployed_at`   | `TIMESTAMP WITH TIME ZONE`| NULLABLE                                             | When was it last successfully deployed.                                                   |

**Indexes for `projects`:**

1.  `PRIMARY KEY (id)` (automatically created)
2.  `CREATE UNIQUE INDEX idx_projects_unique_key ON projects (unique_key);` (For fast lookup by URL)
3.  `CREATE INDEX idx_projects_user_id ON projects (user_id);` (Crucial for user-specific queries)
4.  `CREATE INDEX idx_projects_status ON projects (status);` (To quickly find projects by their current status)

---

#### ⚙️ `project_settings` Table

This table will store flexible settings for each project, using JSONB for maximum flexibility.

| Column Name     | Data Type                 | Constraints/Notes                          | Performance Notes                                                                                             |
| :-------------- | :------------------------ | :----------------------------------------- | :------------------------------------------------------------------------------------------------------------ |
| `project_id`    | `BIGINT`                  | PRIMARY KEY, FOREIGN KEY (projects.id) ON DELETE CASCADE | Corresponds to `projects.id`. ON DELETE CASCADE ensures settings are removed if a project is deleted. |
| `settings`      | `JSONB`                   | NOT NULL DEFAULT '{}'::jsonb               | Stores key-value pairs for project configuration (e.g., `{"NODE_ENV": "production", "PORT": 3000}`). |
| `updated_at`    | `TIMESTAMP WITH TIME ZONE`| NOT NULL DEFAULT NOW()                     | Tracks last time settings were modified.                                                                      |

**Indexes for `project_settings`:**

*   `PRIMARY KEY (project_id)` (automatically created)
*   **GIN Index on `settings`:**
    `CREATE INDEX idx_project_settings_settings ON project_settings USING GIN (settings);`
    This is *essential* for fast querying of specific keys or values within the JSONB `settings` column.

---

#### 👤 `users` Table (Minimal for now)

You'll need a `users` table to link projects to owners.

| Column Name     | Data Type                 | Constraints/Notes                | Performance Notes                                   |
| :-------------- | :------------------------ | :------------------------------- | :-------------------------------------------------- |
| `id`            | `BIGSERIAL`               | PRIMARY KEY                      |                                                     |
| `username`      | `VARCHAR(255)`            | UNIQUE, NOT NULL                 | For login, identification. **Index this column**. |
| `password_hash` | `VARCHAR(255)`            | NOT NULL                         | Store hashed passwords only.                        |
| `created_at`    | `TIMESTAMP WITH TIME ZONE`| NOT NULL DEFAULT NOW()           |                                                     |

**Indexes for `users`:**

1.  `PRIMARY KEY (id)` (automatically created)
2.  `CREATE UNIQUE INDEX idx_users_username ON users (username);` (For fast login/lookup)

---

## 🧱 Phase 2: Project Structure (Clean Architecture)

Clean Architecture promotes a layered design, separating concerns and making the application more maintainable, testable, and independent of frameworks/databases.

**Core Principles:**

*   **Entities:** Core business objects (e.g., `Project`).
*   **Use Cases (Interactors):** Application-specific business rules. They orchestrate the flow of data to and from entities.
*   **Interface Adapters:** Convert data between the format convenient for use cases and the format convenient for external agencies (DB, UI, etc.). This layer includes Presenters, Controllers, Gateways (Repository interfaces).
*   **Frameworks & Drivers:** Outermost layer. Databases, web frameworks, UI.

**Proposed Layers in Go:**

```
your_app/
├── cmd/
│   └── runner/          # Main application entry point
│       └── main.go      # Initializes everything, starts HTTP server
├── internal/
│   ├── domain/          # Core business logic, Entities, Domain Events, Interfaces (e.g., ProjectRepository)
│   │   ├── entity/
│   │   │   └── project.go     # Project struct, status constants etc.
│   │   ├── repository/
│   │   │   └── project_repo.go # ProjectRepository interface definition
│   │   └── service/
│   │       └── deploy_service.go # Core deployment logic
│   ├── usecase/         # Application-specific business logic (interactors)
│   │   ├── upload_project.go
│   │   ├── get_project_settings.go
│   │   ├── run_project.go
│   │   └── stop_project.go
│   ├── infrastructure/  # External concerns: DB, HTTP, external APIs
│   │   ├── database/
│   │   │   ├── postgres/
│   │   │   │   ├── project_repo_impl.go # PostgreSQL implementation of ProjectRepository
│   │   │   │   └── models.go          # DB specific models (if needed, or map from domain)
│   │   │   └── repository.go      # Database connection setup
│   │   ├── http/
│   │   │   ├── server.go          # HTTP server setup
│   │   │   ├── handlers/          # HTTP handlers for endpoints
│   │   │   │   ├── upload_handler.go
│   │   │   │   ├── settings_handler.go
│   │   │   │   └── run_handler.go
│   │   │   └── router.go          # Routing setup
│   │   └── subprocess/       # For running npm, node, docker commands
│   │       └── command_runner.go # Interface & Implementation for command execution
│   └── pkg/             # Common utilities, shared code across internal layers
│       └── errors/
│           └── errors.go
└── go.mod
```

### 2.1 Domain Layer (`internal/domain`)

*   **Entities:**
    *   `Project` struct: `ID`, `UserID`, `Name`, `UniqueKey`, `SourceType`, `SourceLocation`, `Status`, `Port`, `CreatedAt`, `UpdatedAt`, `DeployedAt`.
    *   Define constants for `ProjectStatus` (e.g., `Pending`, `Building`, `Running`, `Failed`, `Stopped`).
*   **Interfaces:**
    *   `ProjectRepository`: Define methods like `Save(ctx context.Context, p *entity.Project) error`, `FindByID(ctx context.Context, id string) (*entity.Project, error)`, `FindByUniqueKey(ctx context.Context, key string) (*entity.Project, error)`, `UpdateStatus(ctx context.Context, id string, status entity.ProjectStatus) error`, `SetPort(ctx context.Context, id string, port int) error`.
    *   `CommandRunner`: Define `Run(ctx context.Context, command CommandConfig) (Output, error)`. `CommandConfig` would have `Dir`, `Env`, `Args`. `Output` would have `Stdout`, `Stderr`, `ExitCode`.

### 2.2 Use Case Layer (`internal/usecase`)

*   **`UploadProject` Use Case:**
    *   Input: `UserID`, `ProjectName`, `File (io.Reader)`, `SourceType`.
    *   Logic:
        1.  Validate inputs.
        2.  Generate `UniqueKey`.
        3.  Save source file to a temporary location or S3 (later).
        4.  Create a new `entity.Project` in `Pending` status.
        5.  Call `ProjectRepository.Save()`.
        6.  Initiate asynchronous build/deploy process (e.g., send a message to a queue or trigger a separate goroutine).
    *   Output: `ProjectID`, `UniqueKey`.

*   **`RunProject` Use Case:**
    *   Input: `ProjectID`.
    *   Logic:
        1.  Fetch `entity.Project` by `ProjectID`.
        2.  If project is `Stopped` or `Failed` and has a source location:
            *   Pick a free port.
            *   Use `CommandRunner` to `npm install` and `npm start` (or `node server.js`) in project's directory.
            *   Update `Project` status to `Running` and set `Port`.
            *   Call `ProjectRepository.UpdateStatus()` and `ProjectRepository.SetPort()`.
            *   Store/track the running process.
        3.  Return the access URL (e.g., `http://localhost:<port>` or your proxy domain).

*   **`GetProjectSettings` Use Case:**
    *   Input: `ProjectID`.
    *   Logic: Fetch `Project` and its settings from DB.
    *   Output: Project details and settings.

### 2.3 Infrastructure Layer (`internal/infrastructure`)

*   **Database Implementation:**
    *   `postgres.ProjectRepositoryImpl` implements `domain.ProjectRepository`. Uses `database/sql` or `pgx` to interact with PostgreSQL.
    *   Handles `BIGSERIAL` to Go `int64`, `TIMESTAMP WITH TIME ZONE` to `time.Time`, `JSONB` to Go `map[string]interface{}` or a custom struct.
*   **HTTP Handlers:**
    *   `upload_handler`: Parses multipart form, calls `usecase.UploadProject`.
    *   `settings_handler`: Fetches project and settings, returns them. For PUT/POST, it would call a `UpdateProjectSettings` use case.
    *   `run_handler`: Calls `usecase.RunProject`, returns the access URL.
*   **Command Runner Implementation:**
    *   `subprocess.CommandRunnerImpl` implements `domain.CommandRunner`. Uses `os/exec` package.

### 2.4 Entry Point (`cmd/runner/main.go`)

*   Set up database connection (`infrastructure/database`).
*   Create repository implementations (`infrastructure/postgres`).
*   Create command runner implementation (`infrastructure/subprocess`).
*   Inject dependencies (repositories, runners) into use cases.
*   Set up HTTP router (`infrastructure/http/router.go`) and handlers, injecting use cases into them.
*   Start the HTTP server.

---

## 💡 Performance Considerations for 100k Users / 10k Projects

1.  **Database Indexing:** As detailed above, proper indexing is paramount. Use `EXPLAIN ANALYZE` in PostgreSQL to see how your queries are performing.
2.  **Connection Pooling:** Use `database/sql.DB` or `pgxpool.Pool` for efficient management of database connections.
3.  **Read Replicas (Future):** If read load becomes extreme, consider setting up read replicas.
4.  **Caching (Redis):**
    *   Cache frequently accessed, rarely changing data (e.g., project metadata if it's not updated often).
    *   Cache results of expensive computations or queries.
    *   Use Redis for short-lived ephemeral data, like rate limiting or temporary job queues.
    *   **For your `/run/:id` endpoint:** You could cache the mapping of `unique_key` to `port` in Redis for lightning-fast lookups, reducing DB load.
5.  **Asynchronous Operations:** The build and deploy process *must* be asynchronous. You upload, get an ID, and then deploy happens in the background. This is where message queues (like RabbitMQ or Kafka) or even just Go goroutines with channels come into play for managing long-running tasks.
6.  **Stateless Services:** Your Go application should be stateless. All state should be in the DB or Redis. This allows you to run multiple instances of your Go app behind a load balancer.
7.  **Efficient Data Transfer:** Use PostgreSQL's `COPY` command for bulk inserts/updates if you ever need to import large amounts of data.
8.  **JSONB vs. Relational:** For `project_settings`, JSONB is great for flexibility. If you find yourself querying very specific, nested parts of settings very often and performance becomes an issue, you *could* normalize them into separate tables, but start with JSONB.

---

This is a significant architectural shift. Take it step by step.

**Next Actions:**

1.  **Set up PostgreSQL:** Install it or spin it up with Docker.
2.  **Create Tables:** Run SQL scripts to create the `users`, `projects`, and `project_settings` tables with the defined indexes.
3.  **Define Domain Entities & Interfaces:** Start modeling your `Project` entity and the `ProjectRepository` interface in `internal/domain`.
4.  **Implement Repository:** Write the `postgres.ProjectRepositoryImpl` using `pgx` or `database/sql`.
5.  **Refactor Upload:** Adapt the existing upload logic to use your new domain interfaces and repository.

Let me know which part you want to dive into first!