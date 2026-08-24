###  cineplex

A lightweight, cloud-native internal service designed to broadcast updates about currently running films in cineplex.md. 
Leveraging a pub/sub model, it ensures timely notifications are delivered to all subscribed services within reeldex ecosystem.

#### project structure is layered

```
project
├── cmd                      # Command-related files
│   └── app                  # Application entry point
│       └── main.go          # Main application logic
├── internal                 # Internal codebase
│   ├── handlers             # HTTP request handlers (controllers)
│   │   └── user_handler.go  # User-specific handler
│   ├── services             # Business logic (service layer)
│   │   └── user_service.go  # User-specific service
│   ├── repositories         # Data access (repository layer)
│   │   └── user_repo.go     # User-specific repository
│   └── models               # Data models (entities)
│       └── user.go          # User model
├── pkg                      # Shared utilities or helpers
├── configs                  # Configuration files
├── go.mod                   # Go module definition
└── go.sum                   # Go module checksum file
```


Taken from:
https://medium.com/@smart_byte_labs/organize-like-a-pro-a-simple-guide-to-go-project-folder-structures-e85e9c1769c2


#### Tools used:
- air https://github.com/air-verse/air
- direnv https://github.com/direnv/direnv/blob/master/docs/installation.md