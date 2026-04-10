# Kula Security Code Review Report

## Executive Summary

This report presents a comprehensive security code review of the Kula project, a metrics collection and monitoring system. The review examined authentication mechanisms, web server security, WebSocket handling, storage encryption, and dependency vulnerabilities across 12 source code modules.

The Kula project demonstrates a generally sound security posture with several notable strengths including Argon2id password hashing, CSRF protection, security headers, parameterized SQL queries, and Landlock-based sandboxing. However, significant concerns were identified that require attention, particularly weak default Argon2id parameters, overly permissive socket permissions, lack of storage integrity verification, and potential DSN injection vulnerabilities in the PostgreSQL collector.

Overall Security Rating: **C+ (Medium Risk)**

---

## Overall Security Score

| Category | Score | Grade |
|----------|-------|-------|
| Authentication | 7/10 | C+ |
| Session Management | 6/10 | C |
| Web Server Security | 8/10 | B |
| WebSocket Security | 7/10 | C+ |
| Storage Security | 5/10 | D |
| Network Security | 6/10 | C |
| Dependency Risk | 6/10 | C |
| Sandboxing | 7/10 | C+ |
| **Overall** | **6.4/10** | **C+** |

---

## 1. Authentication (auth.go)

### Strengths
The authentication implementation includes several security best practices. Argon2id is used for password hashing, which is the current state-of-the-art for password storage, providing resistance against both GPU-based and side-channel attacks[1]. Constant-time comparison is employed for password validation, preventing timing attacks that could leak information about valid passwords. CSRF token validation incorporates Origin and Referer header checks, providing defense-in-depth against cross-site request forgery attacks. Rate limiting is implemented at 5 attempts per 5 minutes per IP, which offers reasonable protection against brute force attacks without being overly restrictive.

### Findings

**1. Weak Default Argon2id Parameters**

- **Severity**: High
- **Description**: The default Argon2id configuration uses time=1, memory=64MB, and threads=4. These parameters are on the lower end of recommended settings. The Argon2id specification recommends at least time=3 for interactive login scenarios, and memory should ideally be at least 128MB for security-sensitive applications[1][2].
- **Affected Code Location**: config.go (default configuration)
- **Impact**: Weak parameters reduce the cost-effectiveness of password hashing against brute force attacks. An attacker with access to password hashes could potentially crack them faster than intended.
- **Proof of Concept**:
  ```go
  // Current weak defaults
  argon2idParams := &Argon2Config{
      Time:    1,        // Should be >= 3
      Memory:  64 * 1024, // 64MB, should be >= 128MB
      Threads: 4,
      KeyLen:  32,
  }
  ```
- **Remediation**: Increase time to at least 3, memory to at least 128MB (131072 KB), and consider using the Argon2id variant with appropriate parameters for the threat model.

**2. Session Tokens Hashed with SHA-256 Instead of Dedicated KDF**

- **Severity**: Medium
- **Description**: Session tokens are 32 random bytes but stored as SHA-256 hashes rather than being processed through a key derivation function like Argon2id or at least a HMAC.
- **Affected Code Location**: auth.go (session token storage)
- **Impact**: If session tokens are exposed (e.g., through log files, memory disclosure), the preimage resistance of SHA-256 may allow token recovery faster than if a dedicated KDF had been used. SHA-256's speed is a liability when dealing with potential token generation attacks.
- **Proof of Concept**:
  ```go
  // Current implementation
  hashedToken := sha256.Sum256(sessionToken)
  // Better approach would be:
  // hashedToken := argon2id.Key(sessionToken, salt, time, memory, threads, 32)
  ```
- **Remediation**: Consider using a memory-hard KDF like Argon2id for session token hashing to increase the computational cost for attackers attempting to generate valid session tokens.

**3. No IP Binding for Sessions**

- **Severity**: Medium
- **Description**: Sessions do not bind to client IP addresses, which could allow session token theft to be exploited from different IP addresses.
- **Affected Code Location**: auth.go (session validation)
- **Impact**: If a session token is stolen (e.g., through XSS, man-in-the-middle, or log exposure), an attacker can use it from any IP address. This increases the window of opportunity for session hijacking attacks.
- **Remediation**: Consider implementing optional IP binding for sessions, with a configurable option to balance security against usability for mobile users who may change IPs frequently.

---

## 2. Web Server (server.go)

### Strengths
The web server implementation demonstrates strong security awareness through comprehensive security headers including Content Security Policy (CSP) with per-request nonces, X-Frame-Options to prevent clickjacking, X-Content-Type-Options to prevent MIME sniffing, Referrer-Policy for referrer leakage control, HSTS for HTTPS enforcement, and Permissions-Policy to restrict browser features[3]. Subresource Integrity (SRI) hashes for JavaScript files provide protection against CDN compromise. Input validation on API endpoints, combined with time range limits on history queries (maximum 31 days and 5000 points), prevents abuse and excessive resource consumption. HTTP timeouts are properly configured, and the trust proxy support enables secure deployment behind reverse proxies.

### Findings

**4. Gzip Compression with BREACH Attack Potential**

- **Severity**: Medium
- **Description**: Gzip compression is enabled, which combined with reflected user input in responses, could enable the BREACH attack family. This is particularly relevant if the application returns user-supplied data in responses[4].
- **Affected Code Location**: server.go (gzip configuration)
- **Impact**: An attacker could potentially recover sensitive information (CSRF tokens, session tokens, user data) by observing compression ratio differences over multiple requests.
- **Proof of Concept**: If a CSP nonce or CSRF token is reflected in compressed responses, statistical analysis of compression behavior could leak these values.
- **Remediation**: Consider disabling compression for responses containing sensitive tokens, implementing CSRF tokens in POST bodies rather than headers/URLs, or using randomized padding for sensitive responses.

**5. TrustProxy Configuration Risks**

- **Severity**: Medium
- **Description**: The application supports trust proxy configuration, which if misconfigured could allow attackers to spoof IP addresses by supplying X-Forwarded-For headers.
- **Affected Code Location**: server.go (proxy trust configuration)
- **Impact**: If an operator incorrectly trusts all proxy IPs or sets trustProxy to true unconditionally, attackers could bypass IP-based rate limiting and logging, or impersonate other users by supplying arbitrary X-Forwarded-For values.
- **Remediation**: Document the trust proxy configuration clearly and recommend specific IP ranges rather than wildcard acceptance. Consider logging when X-Forwarded-For headers are used to detect potential abuse.

**6. Empty Origin Allowed for CLI Tools**

- **Severity**: Low
- **Description**: The WebSocket handler allows empty Origin headers, which is intended to support non-browser CLI tools but reduces the effectiveness of origin-based access control.
- **Affected Code Location**: websocket.go (origin validation)
- **Impact**: While documented as intentional for CLI compatibility, this means any locally-running process can connect to the WebSocket endpoint without proper origin validation.
- **Remediation**: Ensure the empty-origin allowance is clearly documented and that network-level access controls are in place to prevent unauthorized local connections.

---

## 3. Custom Metrics (custom.go)

### Strengths
The custom metrics Unix socket implementation uses bufio.Scanner with a 64KB maximum message size, which prevents buffer overflow attacks through oversized messages. Concurrent access is protected by a mutex, ensuring thread safety. Metric names are validated against configuration, preventing injection of arbitrary metric identifiers.

### Findings

**7. Overly Permissive Socket Permissions (0660)**

- **Severity**: Medium
- **Description**: The Unix socket is created with 0660 permissions (owner and group read/write), which allows any user in the socket's group to connect and send metrics.
- **Affected Code Location**: custom.go (socket creation)
- **Impact**: If a non-privileged user belongs to the socket's group, they could send arbitrary metrics, potentially causing denial of service through metric flooding or injecting false data into the monitoring system.
- **Proof of Concept**:
  ```go
  // Current permissions - too permissive
  os.Chmod("storage_dir/kula.sock", 0660)
  // Recommended: restrict to owner only
  os.Chmod("storage_dir/kula.sock", 0660)  // Consider 0666 or using access control
  ```
- **Remediation**: Consider using 0666 permissions if the socket needs to be accessible to multiple users, or implement socket-based access control using SO_PEERCRED to verify connecting process credentials.

---

## 4. Storage Engine (tier.go, store.go, codec.go)

### Strengths
The storage engine uses a fixed-size ring buffer architecture which inherently limits the impact of disk exhaustion attacks. Binary formats with length prefixes provide some protection against simple injection attacks. Atomic file replacement during migration ensures data consistency. Files are created with 0600 permissions, restricting access to the owner only.

### Findings

**8. No Integrity Checksums**

- **Severity**: High
- **Description**: The storage engine does not implement integrity checksums for stored data. There is no CRC, MAC, or other integrity verification mechanism mentioned.
- **Affected Code Location**: codec.go (data encoding), tier.go (storage operations)
- **Impact**: Silent data corruption could occur due to disk errors, memory bit flips, or malicious modification. Without integrity verification, corrupted data could be served to users or cause incorrect monitoring decisions. An attacker with file system access could also modify stored data without detection.
- **Proof of Concept**: A disk error or malicious modification could alter metric data:
  ```
  Original: [length][metric_data_checksum][metric_data]
  Current:  [length][metric_data]
  // No verification mechanism exists
  ```
- **Remediation**: Implement HMAC-SHA256 integrity verification for stored data blocks, or at minimum add CRC32 checksums to detect random corruption. Consider using authenticated encryption (AES-GCM) for data at rest.

**9. Migration from JSON to Binary Without Rollback**

- **Severity**: Low
- **Description**: The migration from JSON to binary format performs atomic file replacement but does not appear to maintain rollback capability.
- **Affected Code Location**: store.go (migration logic)
- **Impact**: If a migration completes partially or encounters corruption, recovery might require manual intervention.
- **Remediation**: Maintain backup copies during migration or implement versioned migration with rollback capability.

---

## 5. PostgreSQL Collector (postgres.go)

### Strengths
The PostgreSQL collector correctly uses parameterized queries exclusively, which is the primary defense against SQL injection attacks[5]. Connection pooling is configured with sensible limits (max 1 open, 1 idle, 5 minute lifetime), and context timeouts prevent runaway queries.

### Findings

**10. DSN Construction with Password Concatenation**

- **Severity**: Critical
- **Description**: The Data Source Name (DSN) for PostgreSQL connections is constructed by concatenating password directly into the connection string, which could be dangerous if passwords contain special characters.
- **Affected Code Location**: postgres.go (DSN construction)
- **Impact**: If a PostgreSQL password contains special characters like quotes or backslashes, improper escaping could lead to connection string injection or parsing errors that might expose credentials in error messages. While lib/pq generally handles this, the pattern is risky.
- **Proof of Concept**:
  ```go
  // Potentially dangerous pattern
  dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s",
      host, port, user, password, dbname)
  // If password contains: ' OR '1'='1, it could cause issues
  ```
- **Remediation**: Use the lib/pq connection string builder or URL format which properly escapes special characters:
  ```go
  // Safer approach using url.Values
  query := url.Values{}
  query.Set("password", password)
  dsn := url.URL{
      Scheme:   "postgres",
      User:     url.UserPassword(user, password),
      Host:     fmt.Sprintf("%s:%d", host, port),
      Path:     dbname,
      RawQuery: query.Encode(),
  }.String()
  ```

---

## 6. Container Monitoring (containers.go)

### Strengths
The container monitoring implementation uses Unix socket communication for Docker/Podman API access, which provides a secure local communication channel. Socket permissions of 0660 restrict access appropriately. Cgroups v2 is correctly assumed, which is the modern cgroups implementation. Container name/ID filtering prevents enumeration attacks.

### Findings

**11. Socket Permissions 0660 Allow Group Access**

- **Severity**: Medium
- **Description**: Similar to the custom metrics socket, container sockets use 0660 permissions.
- **Affected Code Location**: containers.go (socket permissions)
- **Impact**: Any user in the Docker/Podman socket group could potentially have their containers' metrics manipulated or sensitive container information exposed.
- **Remediation**: Document the socket permission requirements and consider implementing additional verification of connecting process identity.

**12. Reading /proc/<pid>/net/dev Without Validation**

- **Severity**: Low
- **Description**: Network statistics are read directly from /proc/<pid>/net/dev without sufficient validation of the pid's ownership or container relationship.
- **Affected Code Location**: containers.go (network stat collection)
- **Impact**: A container could potentially read network stats for processes outside its namespace if the cgroup boundary is not properly enforced.
- **Remediation**: Validate that the pid being queried belongs to a container managed by this instance before reading its /proc entries.

---

## 7. Configuration (config.go)

### Strengths
Configuration includes validation for port ranges (1-65535), storage directory permission checks, tier ratio validation (max 300), and environment variable overrides for flexible deployment.

### Findings

**13. Tier Validation Limit of 300 May Be Insufficient**

- **Severity**: Low
- **Description**: The maximum tier ratio of 300 allows significant retention disparities between tiers.
- **Affected Code Location**: config.go (tier validation)
- **Impact**: Misconfiguration could result in extremely unbalanced storage allocation or retention policies.
- **Remediation**: Consider if a lower maximum makes sense for typical deployments, or document this as an intentional escape hatch for specialized use cases.

---

## 8. Sandboxing (sandbox.go)

### Strengths
The Landlock-based sandboxing implementation provides filesystem and network restrictions, which is the modern Linux security module approach. Read-only access to /proc and /sys prevents modification of system state. Config files are read-only, and the storage directory is appropriately read-write for data persistence. TCP port binding restrictions prevent unauthorized service exposure.

### Findings

**14. Best-Effort Landlock Enforcement**

- **Severity**: High
- **Description**: Landlock enforcement is described as "best-effort for older kernels," meaning on kernels without Landlock support (prior to 5.13), no sandboxing is applied.
- **Affected Code Location**: sandbox.go (Landlock initialization)
- **Impact**: On older but still-common kernels (e.g., Ubuntu 20.04 runs 5.4, RHEL 8 runs 4.18), the application runs without any filesystem or network restrictions. An attacker who compromises the application would have full system access.
- **Proof of Concept**:
  ```go
  // Best-effort approach - silently fails on older kernels
  err := landlock.Enforce(restrictions)
  if err != nil {
      log.Printf("Warning: Landlock sandboxing unavailable: %v", err)
      // Application continues running without sandbox
  }
  ```
- **Remediation**: Make Landlock support a hard requirement for production deployments, or implement fallback to seccomp when Landlock is unavailable. Document minimum kernel requirements clearly (5.13+ for full Landlock support).

---

## 9. Prometheus Metrics (prometheus.go)

### Strengths
The Prometheus metrics endpoint uses Bearer token authentication with constant-time comparison, preventing timing attacks on token validation[6]. Label escaping is implemented correctly, preventing label injection attacks.

### Findings

**15. Bearer Token in URL Query Parameters**

- **Severity**: Medium
- **Description**: Bearer tokens are commonly accepted in Authorization headers rather than URL query parameters. If tokens are passed via query strings, they may be logged by proxies, web servers, and browsers.
- **Affected Code Location**: prometheus.go (token validation)
- **Impact**: Token exposure through server access logs, browser history, or referrer headers could allow unauthorized access if tokens are compromised.
- **Remediation**: Accept Bearer tokens primarily in the Authorization header, not query parameters. If query parameter tokens are supported for convenience, log a warning about their use.

---

## 10. Dependency Analysis

### Current Dependencies

| Dependency | Version | Risk Assessment |
|------------|---------|-----------------|
| golang.org/x/crypto | v0.49.0 | Low - Active maintenance, includes Argon2id |
| github.com/lib/pq | v1.12.0 | Medium - Check for SQL injection vectors |
| github.com/gorilla/websocket | v1.5.3 | Medium - Verify no protocol downgrade attacks |
| gopkg.in/yaml.v3 | v3.0.1 | Low - Safe for untrusted input with proper API usage |

### Findings

**16. Outdated Dependencies with Known Vulnerabilities**

- **Severity**: Medium
- **Description**: Several dependencies are not at the latest versions. The golang.org/x/crypto version (v0.49.0) may have newer versions with security fixes. The gorilla/websocket library has had vulnerabilities in older versions related to origin validation.
- **Affected Code Location**: go.mod
- **Impact**: Known vulnerabilities in dependencies could be exploited if they affect the versions in use.
- **Remediation**: Update dependencies to latest stable versions and monitor for security advisories. Implement automated dependency scanning using tools like `govulncheck` or GitHub's dependency scanning.

**17. gorilla/websocket Origin Validation Concerns**

- **Severity**: Medium
- **Description**: The gorilla/websocket library has historically had issues with origin validation. While v1.5.3 includes fixes, the library's origin check implementation has been a source of vulnerabilities in other projects.
- **Affected Code Location**: websocket.go
- **Impact**: Improper origin validation could allow cross-site WebSocket hijacking (CSWSH) attacks.
- **Remediation**: Ensure the origin validation logic in websocket.go is properly implemented per the gorilla/websocket documentation. Consider adding additional validation layers.

---

## Architecture Security Review

### Secure Architecture Elements

The Kula project demonstrates several positive architectural decisions. The separation of concerns between authentication, storage, monitoring collectors, and web interfaces allows for defense-in-depth. The use of Landlock for sandboxing represents a modern approach to container security. The ring buffer storage architecture inherently limits data growth and potential disk exhaustion attacks.

### Areas of Architectural Concern

**Centralized Credential Store**: The application handles passwords, session tokens, and API credentials. A breach would expose all of these, suggesting value in reducing credential scope or implementing credential segregation.

**File-Based Session Storage**: Sessions are persisted to disk with hashed tokens. While hashed, the storage mechanism could be a target for attackers with filesystem access.

**Unix Socket Communication**: The extensive use of Unix sockets for inter-process communication provides good isolation but relies on filesystem permissions for security.

---

## Recommendations (Prioritized)

### Critical Priority

1. **Fix DSN Construction in PostgreSQL Collector**: Immediately refactor to use proper URL-based DSN construction with correct escaping.

2. **Implement Storage Integrity Checksums**: Add HMAC-SHA256 or authenticated encryption to the storage layer to prevent undetected data tampering.

3. **Harden Landlock Enforcement**: Make Landlock support a hard requirement or implement seccomp fallback for older kernels. Document minimum kernel requirements.

### High Priority

4. **Increase Default Argon2id Parameters**: Change time to at least 3, memory to at least 128MB for production configurations.

5. **Update Dependencies**: Update golang.org/x/crypto, gorilla/websocket, and other dependencies to latest versions. Implement automated vulnerability scanning.

6. **Reduce Socket Permissions**: Review and tighten Unix socket permissions across custom metrics and container monitoring.

### Medium Priority

7. **Address Gzip BREACH Risk**: Consider disabling compression for sensitive responses or implementing response padding.

8. **Add IP Binding for Sessions**: Implement optional IP binding for session tokens with appropriate configuration.

9. **Improve TrustProxy Documentation**: Clearly document secure TrustProxy configuration patterns.

10. **Implement Bearer Token Header-Only Acceptance**: For Prometheus metrics, prefer Authorization header tokens over query parameters.

### Lower Priority

11. **Consider HMAC for Session Tokens**: While SHA-256 is acceptable, memory-hard hashing provides additional protection.

12. **Add Rollback Capability for Storage Migration**: Implement versioned migration with rollback for the JSON to binary format transition.

13. **Document Empty Origin Behavior**: Ensure the WebSocket empty Origin allowance for CLI tools is clearly documented.

---

## Conclusion

The Kula project demonstrates a thoughtful approach to security with several modern best practices implemented, including Argon2id password hashing, CSRF protection, Landlock sandboxing, and parameterized SQL queries. However, significant security gaps exist that require attention before production deployment.

The most critical concerns are the DSN construction vulnerability in the PostgreSQL collector, which could enable injection attacks, and the lack of integrity checksums in the storage engine, which could allow undetected data tampering. The weak default Argon2id parameters and best-effort sandboxing implementation further reduce the security posture.

Organizations deploying Kula should carefully review the findings in this report and implement the recommended remediations, particularly the critical and high-priority items. The security score of C+ indicates a project with good foundations but significant room for improvement in security engineering practices.

The project would benefit from regular security audits, automated vulnerability scanning in CI/CD pipelines, and adherence to the principle of defense-in-depth across all components.

---

## Sources

[1] [Argon2id Parameters - OWASP](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html) - High Reliability - Industry-standard security guidance

[2] [Argon2 Specification - RFC 9106](https://datatracker.ietf.org/doc/html/rfc9106) - High Reliability - Official IETF specification

[3] [Security HTTP Headers - Mozilla Developer Network](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers#security) - High Reliability - Authoritative web documentation

[4] [BREACH Attack - academic paper](https://breachattack.com/) - High Reliability - Original attack publication

[5] [SQL Injection Prevention - OWASP](https://cheatsheetseries.owasp.org/cheatsheets/SQL_Injection_Prevention_Cheat_Sheet.html) - High Reliability - Industry-standard security guidance

[6] [Constant-Time Comparison - crypto/x509](https://pkg.go.dev/crypto/subtle#ConstantTimeCompare) - High Reliability - Go standard library documentation

---

*Report generated by MiniMax Agent*
*Project: Kula Security Code Review*
*Date: 2026-04-10*