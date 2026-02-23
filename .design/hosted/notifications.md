# Agent Notifications: Event Subscriptions and Message Dispatch

## Status
**Research & Design** | February 2026

## Problem

When one agent creates another (via `scion start`), the creating agent currently has no way to be notified when the spawned agent reaches a terminal or actionable state — `COMPLETED`, `WAITING_FOR_INPUT`, or `LIMITS_EXCEEDED`. The creating agent must either poll the Hub API or rely on ad-hoc coordination.

This is a critical gap for multi-agent orchestration workflows where a "lead" agent delegates tasks and needs to react when sub-agents finish or need intervention.

### Primary Use Case

```
# bar-agent creates fooagent and subscribes to notifications
scion start --notify bar-agent fooagent "Implement the auth module"

# When fooagent reaches COMPLETED:
# → system sends: scion message bar-agent "fooagent has reached a state of COMPLETED"

# When fooagent reaches WAITING_FOR_INPUT with a question:
# → system sends: scion message bar-agent "fooagent is WAITING_FOR_INPUT: <question>"
```

### Future Extensions

- **Human user notifications** (email, Slack, web push).
- **Stale/stalled detection** — notify when an agent hasn't produced an event within a configurable timeout.
- **`scion get-notified <agent>`** — a separate command for additional actors to subscribe to an already-running agent's events.
- **Multiple subscribers per agent** — initial implementation should store subscribers as a list, even if only one is common at first.

---

## Current Architecture Context

### Event Flow Today

1. **Inside the container**: `sciontool` hooks intercept harness events (Claude Code, Gemini CLI) and translate them into status updates via the Hub API (`POST /api/v1/agents/{id}/status`). See `pkg/sciontool/hooks/handlers/hub.go`.
2. **Hub handler**: `updateAgentStatus()` in `pkg/hub/handlers.go:1212` persists the status change to the store and publishes an event via `EventPublisher`.
3. **EventPublisher** (`pkg/hub/events.go`): `ChannelEventPublisher` fans out to in-process subscribers using NATS-style subject matching. Subjects: `agent.{id}.status`, `grove.{groveId}.agent.status`.
4. **SSE endpoint** (`/events?sub=...`): Browser clients subscribe to events for real-time UI updates.

### Key Status Values (from `pkg/sciontool/hooks/types.go`)

| Status | Sticky? | Notification-Worthy |
|---|---|---|
| `WAITING_FOR_INPUT` | Yes | **Yes** — agent needs human/agent intervention |
| `COMPLETED` | Yes | **Yes** — agent finished its task |
| `LIMITS_EXCEEDED` | Yes | **Yes** — agent hit token/turn limits |
| `THINKING`, `EXECUTING`, `IDLE` | No | No — transient operational states |
| `ERROR` | No | Future consideration |
| `EXITED` | No | Future consideration |

### Messaging Today

`scion message <agent> <text>` sends a message to an agent via the Hub API (`POST /api/v1/agents/{id}/message`), which dispatches through the runtime broker's control channel to inject text into the agent's tmux session.

### Agent-to-Hub Access Today

Agents receive a JWT token (`SCION_HUB_TOKEN`) with scopes. Per the `agent-hub-access.md` design, agents with `grove:agent:create` and `grove:agent:lifecycle` scopes can already create and manage peer agents within their grove. The `scion` CLI inside containers reads `SCION_HUB_ENDPOINT` and `SCION_HUB_TOKEN` to communicate with the Hub.

---

## Design Approaches

### Approach A: Hub-Side Event Listener with Subscription Store

**Summary**: The Hub maintains a `notification_subscriptions` table. When a status event is published via `EventPublisher`, a dedicated notification dispatcher goroutine checks for matching subscriptions and dispatches messages via the existing message API.

#### Data Model

```sql
CREATE TABLE notification_subscriptions (
    id TEXT PRIMARY KEY,
    agent_id TEXT NOT NULL,              -- agent being watched
    subscriber_type TEXT NOT NULL,       -- 'agent' | 'user' (future)
    subscriber_id TEXT NOT NULL,         -- slug or ID of the subscriber
    grove_id TEXT NOT NULL,              -- grove scope for authorization
    trigger_statuses TEXT NOT NULL,      -- JSON array: ["COMPLETED", "WAITING_FOR_INPUT", "LIMITS_EXCEEDED"]
    created_at TIMESTAMP NOT NULL,
    created_by TEXT NOT NULL,            -- who created the subscription
    FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE
);
```

#### Flow

1. **CLI**: `scion start --notify bar-agent fooagent "task..."` → `CreateAgentRequest` includes `NotifySubscribers: [{Type: "agent", ID: "bar-agent"}]`.
2. **Hub** `createAgent` handler: After creating the agent, inserts subscription rows into `notification_subscriptions`.
3. **Hub** `NotificationDispatcher` goroutine: Subscribes to `grove.{groveId}.agent.status` via `ChannelEventPublisher.Subscribe(...)`. On each event:
   - Query `notification_subscriptions` for the agent ID.
   - If the status matches a trigger status, dispatch a message via `POST /api/v1/agents/{subscriberId}/message`.
4. **Cleanup**: Subscriptions are deleted via `ON DELETE CASCADE` when the watched agent is deleted. A background sweep also cleans up subscriptions for deleted subscribers.

#### Architecture Diagram

```
┌──────────────┐     status update     ┌──────────────┐
│  sciontool   │ ──────────────────────>│   Hub API    │
│  (in agent)  │   POST /agents/id/    │  handlers.go │
└──────────────┘      status           └──────┬───────┘
                                              │
                                    store.UpdateAgentStatus()
                                    events.PublishAgentStatus()
                                              │
                                              ▼
                                    ┌──────────────────┐
                                    │  EventPublisher   │
                                    │  (channels)       │
                                    └────┬─────────────┘
                                         │
                            ┌────────────┴────────────┐
                            ▼                         ▼
                   ┌────────────────┐       ┌──────────────────────┐
                   │  SSE endpoint  │       │ NotificationDispatcher│
                   │  (browsers)    │       │  (goroutine)          │
                   └────────────────┘       └──────────┬───────────┘
                                                       │
                                            query subscriptions table
                                            match status → triggers
                                                       │
                                                       ▼
                                            ┌──────────────────┐
                                            │  Hub message API  │
                                            │ POST /agents/     │
                                            │  {sub}/message    │
                                            └──────────────────┘
```

#### Pros

- **Leverages existing infrastructure**: EventPublisher, message API, and control channel all exist and work today.
- **Centralized logic**: All notification matching and dispatch happens in one place (the Hub), making it easy to add new subscriber types (users, webhooks) later.
- **Transactional subscription creation**: Subscriptions are created atomically with agent creation — no race condition between agent starting and subscription registration.
- **Clean lifecycle**: `ON DELETE CASCADE` ensures subscriptions don't outlive the watched agent.
- **Database-backed durability**: Subscriptions survive Hub restarts (the dispatcher re-subscribes to the EventPublisher on startup).

#### Cons

- **Hub becomes message broker**: The Hub gains a new responsibility (dispatch), which adds complexity to what is primarily a state server.
- **Single-node limitation**: `ChannelEventPublisher` is in-process. In a multi-Hub deployment (future), subscriptions on one Hub wouldn't receive events processed by another. This mirrors the existing SSE limitation and would be resolved by the same `PostgresEventPublisher` migration path.
- **Message delivery is best-effort**: If the subscriber agent is stopped or the broker is disconnected, the message is lost. No retry or queue.

---

### Approach B: Polling-Based Notification Service (Sidecar Goroutine)

**Summary**: Instead of subscribing to the in-process event stream, a background goroutine periodically polls the store for agents with active subscriptions whose status matches a trigger condition.

#### Flow

1. **Subscription creation**: Same as Approach A (table + API).
2. **Polling loop**: A `NotificationPoller` goroutine runs every N seconds (e.g., 5s):
   - `SELECT a.id, a.status, a.message, ns.* FROM agents a JOIN notification_subscriptions ns ON a.id = ns.agent_id WHERE a.status IN (trigger_statuses) AND ns.last_notified_status != a.status`
   - For each match, dispatch a message and update `ns.last_notified_status`.
3. **Deduplication**: The `last_notified_status` column prevents repeat notifications for the same status transition.

#### Pros

- **Multi-node safe**: Works correctly in multi-Hub deployments since it polls the shared database rather than relying on in-process events.
- **Simpler event integration**: No dependency on `EventPublisher` subscription machinery; just reads from the database.
- **Naturally idempotent**: The `last_notified_status` column prevents duplicate notifications.
- **Resilient to Hub restarts**: No event subscriptions to re-establish; the next poll cycle catches up.

#### Cons

- **Latency**: Notifications are delayed by up to the poll interval (5-10 seconds). For the use case of notifying an agent that its sub-agent completed, this is likely acceptable — but it's not instant.
- **Database load**: Joins across `agents` and `notification_subscriptions` on every poll interval. With a small number of subscriptions this is negligible; at scale it would need an index and possibly a materialized view.
- **Missed transient states**: If an agent transitions through a trigger status and back before the next poll (unlikely for sticky statuses, but possible), the notification could be missed.
- **Not extensible to real-time**: Can't easily support future real-time notification channels (WebSocket push to users) without adding the event-based path anyway.

---

### Approach C: EventPublisher Decorator (Middleware Pattern)

**Summary**: Wrap the existing `EventPublisher` with a `NotifyingEventPublisher` decorator that intercepts `PublishAgentStatus` calls, checks for matching subscriptions, and dispatches notifications inline before (or after) delegating to the underlying publisher.

#### Implementation Sketch

```go
type NotifyingEventPublisher struct {
    inner       EventPublisher
    store       store.Store
    dispatcher  AgentDispatcher
}

func (n *NotifyingEventPublisher) PublishAgentStatus(ctx context.Context, agent *store.Agent) {
    // Delegate to inner publisher first (SSE, etc.)
    n.inner.PublishAgentStatus(ctx, agent)

    // Check for notification subscriptions
    subs, err := n.store.GetNotificationSubscriptions(ctx, agent.ID)
    if err != nil || len(subs) == 0 {
        return
    }

    for _, sub := range subs {
        if sub.MatchesStatus(agent.Status) {
            go n.dispatchNotification(ctx, sub, agent)
        }
    }
}
```

#### Pros

- **Zero new goroutines**: Notification dispatch piggybacks on the existing event publish path (with async dispatch via goroutines for each notification).
- **Instant**: Notifications are dispatched at the exact moment the status event is published.
- **Transparent**: Existing code that calls `events.PublishAgentStatus()` automatically gains notification capability without changes.
- **Composable**: Additional decorators (logging, metrics, rate limiting) can be stacked.

#### Cons

- **Tight coupling**: Notification logic runs in the request path of status updates. Even with async dispatch, the subscription lookup adds latency to every status update.
- **Error isolation**: If the subscription lookup panics or blocks, it could affect the status update handler (mitigation: catch panics in the decorator).
- **DB call on hot path**: Every `PublishAgentStatus` call now hits the database to check for subscriptions, even when most agents have none. Mitigation: an in-memory cache of "agents with subscriptions" that's invalidated on subscription create/delete.
- **Same single-node limitation as Approach A**: Relies on `ChannelEventPublisher` which is in-process only.

---

## Comparison Matrix

| Criterion | A: Hub Dispatcher | B: Polling | C: Decorator |
|---|---|---|---|
| **Latency** | Near-instant (<100ms) | 5-10s poll interval | Near-instant (<100ms) |
| **Multi-node ready** | No (same as SSE) | Yes | No (same as SSE) |
| **Implementation complexity** | Medium | Low | Low-Medium |
| **New goroutines** | 1 (dispatcher loop) | 1 (poller loop) | 0 (async per notification) |
| **DB queries** | On event (subscriptions only) | On interval (join) | On every status publish |
| **Missed notifications** | No (event-driven) | Possible (transient states) | No (event-driven) |
| **Hub restart resilience** | Re-subscribe on startup | Automatic | Re-wrap on startup |
| **Future extensibility** | High (webhook, email, etc.) | Medium | High (composable) |
| **Separation of concerns** | Good (dedicated component) | Good (separate loop) | Lower (mixed into publisher) |

---

## Recommendation

**Approach A (Hub-Side Event Listener)** is recommended for the initial implementation, with elements of Approach C considered for a future optimization.

Rationale:

1. **The Hub already has all the pieces**: `EventPublisher` with subject-matching, the message API, and the dispatcher for routing to brokers. Approach A composes these existing components with minimal new code.

2. **Near-instant notification matters**: When a sub-agent completes, the lead agent should know immediately so it can integrate the result and continue its own work. A 5-10s polling delay (Approach B) is unnecessarily slow.

3. **Clean separation**: A `NotificationDispatcher` is a clearly-scoped component that can be tested independently, unlike Approach C which mixes notification logic into the publisher.

4. **The single-node limitation is acceptable**: The existing SSE system has the same constraint. The migration path to `PostgresEventPublisher` (described in `web-realtime.md`) will benefit notifications equally when it happens.

5. **Future-proofing**: The subscription table and dispatcher pattern naturally extend to webhooks, email, Slack, and other notification channels. Adding a new `NotificationSink` interface with implementations for each channel is straightforward.

---

## Detailed Design (Approach A)

### 1. Data Model

#### `notification_subscriptions` Table

```sql
CREATE TABLE notification_subscriptions (
    id TEXT PRIMARY KEY,
    agent_id TEXT NOT NULL,                -- Agent being watched
    subscriber_type TEXT NOT NULL DEFAULT 'agent',  -- 'agent' | 'user' (future)
    subscriber_id TEXT NOT NULL,           -- Slug or ID of the subscriber entity
    grove_id TEXT NOT NULL,                -- Grove scope (authorization boundary)
    trigger_statuses TEXT NOT NULL,        -- JSON array, e.g. '["COMPLETED","WAITING_FOR_INPUT","LIMITS_EXCEEDED"]'
    last_notified_status TEXT DEFAULT '',  -- Deduplication: last status that triggered a notification
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_by TEXT NOT NULL,              -- Principal that created the subscription
    FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE
);

CREATE INDEX idx_notification_subs_agent ON notification_subscriptions(agent_id);
CREATE INDEX idx_notification_subs_grove ON notification_subscriptions(grove_id);
```

#### Store Interface Extension

```go
// NotificationSubscriptionStore manages notification subscriptions.
type NotificationSubscriptionStore interface {
    CreateNotificationSubscription(ctx context.Context, sub NotificationSubscription) error
    GetNotificationSubscriptions(ctx context.Context, agentID string) ([]NotificationSubscription, error)
    GetNotificationSubscriptionsByGrove(ctx context.Context, groveID string) ([]NotificationSubscription, error)
    UpdateLastNotifiedStatus(ctx context.Context, subID string, status string) error
    DeleteNotificationSubscription(ctx context.Context, id string) error
    DeleteNotificationSubscriptionsForAgent(ctx context.Context, agentID string) error
}
```

#### Model

```go
type NotificationSubscription struct {
    ID                 string    `json:"id"`
    AgentID            string    `json:"agentId"`            // Agent being watched
    SubscriberType     string    `json:"subscriberType"`     // "agent" | "user"
    SubscriberID       string    `json:"subscriberId"`       // Slug or ID
    GroveID            string    `json:"groveId"`
    TriggerStatuses    []string  `json:"triggerStatuses"`    // e.g. ["COMPLETED", "WAITING_FOR_INPUT"]
    LastNotifiedStatus string    `json:"lastNotifiedStatus"` // Dedup
    CreatedAt          time.Time `json:"createdAt"`
    CreatedBy          string    `json:"createdBy"`
}
```

### 2. CLI Changes

#### `--notify` Flag on `start` Command

**File:** `cmd/start.go`

```go
var notifySubscribers []string

func init() {
    startCmd.Flags().StringArrayVar(&notifySubscribers, "notify", nil,
        "Agent(s) to notify on status changes (COMPLETED, WAITING_FOR_INPUT, LIMITS_EXCEEDED)")
}
```

Usage:
```bash
scion start --notify bar-agent fooagent "Do the thing"
scion start --notify bar-agent --notify baz-agent fooagent "Do the thing"
```

#### Passing to Hub

**File:** `cmd/common.go` — `startAgentViaHub()`

The `--notify` values are added to the `CreateAgentRequest`:

```go
req := &hubclient.CreateAgentRequest{
    Name:               agentName,
    GroveID:            groveID,
    // ... existing fields ...
    NotifySubscribers:  notifySubscribers,
}
```

**File:** `pkg/hubclient/agents.go`

```go
type CreateAgentRequest struct {
    // ... existing fields ...
    NotifySubscribers []string `json:"notifySubscribers,omitempty"` // Agent slugs to notify on key status changes
}
```

### 3. Hub Handler Changes

**File:** `pkg/hub/handlers.go` — `createAgent()`

After successfully creating the agent, if `req.NotifySubscribers` is non-empty:

```go
for _, subscriberSlug := range req.NotifySubscribers {
    sub := store.NotificationSubscription{
        ID:              uuid.New().String(),
        AgentID:         agent.ID,
        SubscriberType:  "agent",
        SubscriberID:    subscriberSlug,
        GroveID:         agent.GroveID,
        TriggerStatuses: []string{"COMPLETED", "WAITING_FOR_INPUT", "LIMITS_EXCEEDED"},
        CreatedBy:       identity.PrincipalID(),
    }
    if err := s.store.CreateNotificationSubscription(ctx, sub); err != nil {
        slog.Warn("Failed to create notification subscription",
            "agentID", agent.ID, "subscriber", subscriberSlug, "error", err)
    }
}
```

### 4. Notification Dispatcher

**New file:** `pkg/hub/notifications.go`

```go
type NotificationDispatcher struct {
    store      store.Store
    events     *ChannelEventPublisher
    dispatcher AgentDispatcher
    stopCh     chan struct{}
}

func NewNotificationDispatcher(store store.Store, events *ChannelEventPublisher, dispatcher AgentDispatcher) *NotificationDispatcher {
    return &NotificationDispatcher{
        store:      store,
        events:     events,
        dispatcher: dispatcher,
        stopCh:     make(chan struct{}),
    }
}

func (n *NotificationDispatcher) Start() {
    // Subscribe to all agent status events across all groves
    ch, unsub := n.events.Subscribe("grove.>.agent.status")

    go func() {
        defer unsub()
        for {
            select {
            case evt, ok := <-ch:
                if !ok {
                    return
                }
                n.handleEvent(evt)
            case <-n.stopCh:
                return
            }
        }
    }()
}

func (n *NotificationDispatcher) handleEvent(evt Event) {
    var statusEvt AgentStatusEvent
    if err := json.Unmarshal(evt.Data, &statusEvt); err != nil {
        return
    }

    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()

    subs, err := n.store.GetNotificationSubscriptions(ctx, statusEvt.AgentID)
    if err != nil || len(subs) == 0 {
        return
    }

    for _, sub := range subs {
        if sub.MatchesStatus(statusEvt.Status) && sub.LastNotifiedStatus != statusEvt.Status {
            go n.dispatch(sub, statusEvt)
        }
    }
}

func (n *NotificationDispatcher) dispatch(sub store.NotificationSubscription, evt AgentStatusEvent) {
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()

    // Build the notification message
    agent, err := n.store.GetAgent(ctx, evt.AgentID)
    if err != nil {
        slog.Warn("Failed to fetch agent for notification", "agentID", evt.AgentID, "error", err)
        return
    }

    var message string
    switch evt.Status {
    case "COMPLETED", "completed":
        message = fmt.Sprintf("%s has reached a state of COMPLETED", agent.Slug)
        if agent.TaskSummary != "" {
            message += ": " + agent.TaskSummary
        }
    case "WAITING_FOR_INPUT", "waiting_for_input":
        message = fmt.Sprintf("%s is WAITING_FOR_INPUT", agent.Slug)
        if agent.Message != "" {
            message += ": " + agent.Message
        }
    case "LIMITS_EXCEEDED", "limits_exceeded":
        message = fmt.Sprintf("%s has reached a state of LIMITS_EXCEEDED", agent.Slug)
        if agent.Message != "" {
            message += ": " + agent.Message
        }
    default:
        message = fmt.Sprintf("%s has reached status: %s", agent.Slug, evt.Status)
    }

    // Dispatch to subscriber
    switch sub.SubscriberType {
    case "agent":
        n.sendAgentMessage(ctx, sub, message)
    // Future: case "user": n.sendUserNotification(...)
    }

    // Update dedup status
    _ = n.store.UpdateLastNotifiedStatus(ctx, sub.ID, evt.Status)
}

func (n *NotificationDispatcher) sendAgentMessage(ctx context.Context, sub store.NotificationSubscription, message string) {
    // Resolve subscriber agent by slug within the grove
    subscriber, err := n.store.GetAgentBySlug(ctx, sub.GroveID, sub.SubscriberID)
    if err != nil {
        slog.Warn("Notification subscriber agent not found",
            "subscriber", sub.SubscriberID, "grove", sub.GroveID, "error", err)
        return
    }

    // Use the dispatcher to send the message via the broker
    if n.dispatcher != nil && subscriber.RuntimeBrokerID != "" {
        if err := n.dispatcher.DispatchAgentMessage(ctx, subscriber, message, false); err != nil {
            slog.Warn("Failed to dispatch notification message",
                "subscriber", sub.SubscriberID, "error", err)
        } else {
            slog.Info("Notification dispatched",
                "from", sub.AgentID, "to", sub.SubscriberID, "status", message)
        }
    }
}

func (n *NotificationDispatcher) Stop() {
    close(n.stopCh)
}
```

### 5. Hub Server Integration

**File:** `pkg/hub/server.go`

```go
func (s *Server) Start() error {
    // ... existing initialization ...

    // Start notification dispatcher
    if ep, ok := s.events.(*ChannelEventPublisher); ok {
        s.notificationDispatcher = NewNotificationDispatcher(s.store, ep, s.GetDispatcher())
        s.notificationDispatcher.Start()
    }

    // ... existing start logic ...
}
```

### 6. Status Normalization

The Hub receives status values in different cases from different sources. The notification system needs to handle both:
- Uppercase from `sciontool` hooks: `COMPLETED`, `WAITING_FOR_INPUT`, `LIMITS_EXCEEDED`
- Lowercase from Hub internals: `completed`, `waiting_for_input`, `limits_exceeded`

The `MatchesStatus` method on `NotificationSubscription` should normalize case:

```go
func (s *NotificationSubscription) MatchesStatus(status string) bool {
    normalized := strings.ToUpper(status)
    for _, trigger := range s.TriggerStatuses {
        if strings.ToUpper(trigger) == normalized {
            return true
        }
    }
    return false
}
```

### 7. New Agent Token Scope

Add a scope for notification management:

```go
ScopeAgentNotify AgentTokenScope = "grove:agent:notify"
```

This scope allows an agent to create notification subscriptions for agents within its grove. It should be auto-granted alongside `grove:agent:create` and `grove:agent:lifecycle` (since the primary use case is agents that spawn sub-agents).

### 8. Local-Mode Considerations

The `--notify` flag is **Hub-only** in the initial implementation. In local mode (no Hub), the flag should produce a clear error:

```
Error: --notify requires Hub mode. Enable Hub integration or use --hub <endpoint>.
```

Future work could implement local-mode notifications via a lightweight file-based or socket-based mechanism, but this is out of scope.

---

## Message Format

Notifications are delivered as plain-text messages via `scion message`. The format is designed to be parseable by LLMs (since the primary subscriber is another agent):

| Status | Message Format |
|---|---|
| `COMPLETED` | `{agent-slug} has reached a state of COMPLETED` or `{agent-slug} has reached a state of COMPLETED: {taskSummary}` |
| `WAITING_FOR_INPUT` | `{agent-slug} is WAITING_FOR_INPUT: {message/question}` |
| `LIMITS_EXCEEDED` | `{agent-slug} has reached a state of LIMITS_EXCEEDED: {message}` |

---

## Open Questions

### 1. Should `--notify` accept agent slugs or IDs?

**Recommendation**: Slugs. They're human-readable and what users type in all other CLI commands. The Hub resolves them to IDs internally. If the subscriber agent doesn't exist yet at creation time, the subscription is stored by slug and resolved at dispatch time — this supports scenarios where both agents are started in parallel.

### 2. Should subscriptions auto-expire?

**Recommendation**: Yes, via `ON DELETE CASCADE` on the watched agent. When the watched agent is deleted, its subscriptions are cleaned up. Additionally, a TTL-based expiration (e.g., 24 hours) could prevent stale subscriptions from accumulating, but this is a low priority since the cascade handles the common case.

### 3. Should the notification message interrupt the subscriber agent?

**Recommendation**: No, not by default. The `interrupt` flag on `scion message` forcibly interrupts the agent's current work. Notifications should be queued as regular messages that the agent processes when it reaches its next prompt. A future `--notify-interrupt` flag could be added for urgent notifications.

### 4. What happens if the subscriber agent is stopped?

The message dispatch will fail (the broker can't inject text into a stopped tmux session). The notification is logged and dropped. If retry/queuing is needed, it would require a message queue — which is out of scope for the initial implementation.

### 5. Should there be a REST API for managing subscriptions?

**Recommendation**: Yes, but it can follow the initial implementation. The minimum viable API:

```
POST   /api/v1/agents/{id}/subscriptions          — Create subscription
GET    /api/v1/agents/{id}/subscriptions          — List subscriptions
DELETE /api/v1/agents/{id}/subscriptions/{subId}  — Delete subscription
```

For the initial implementation, subscriptions are created implicitly via the `--notify` flag on `CreateAgentRequest`. The explicit API enables the future `scion get-notified <agent>` command.

### 6. Cross-grove notifications?

**Recommendation**: Not in the initial implementation. Both the watched agent and the subscriber must be in the same grove. This aligns with the grove isolation boundary enforced by agent JWT scopes.

### 7. Status case normalization strategy?

The codebase uses both uppercase (`COMPLETED` from hooks/types.go `AgentState` constants) and lowercase (`completed` from `pkg/sciontool/hub/client.go` `AgentStatus` constants). The Hub store appears to persist whatever case it receives. The notification system should normalize via `strings.ToUpper()` for matching, but this highlights a broader codebase inconsistency that may be worth addressing separately.

### 8. Stale/stalled detection

The user mentioned wanting to detect when an agent is "stalled" (no events for a configurable period). This could be implemented as:
- A periodic check in the `NotificationDispatcher` that compares `agent.LastSeen` against a threshold (e.g., 10 minutes).
- If exceeded and the agent has active subscriptions with a `STALLED` trigger, dispatch a notification.

This is a natural extension of the dispatcher pattern but is deferred to a future iteration.

---

## Implementation Plan

### Phase 1: Core Infrastructure
1. Add `notification_subscriptions` table (new SQLite migration).
2. Add `NotificationSubscriptionStore` interface and SQLite implementation.
3. Add `NotificationSubscription` model to `pkg/store/models.go`.

### Phase 2: Notification Dispatcher
4. Implement `NotificationDispatcher` in `pkg/hub/notifications.go`.
5. Wire dispatcher into Hub server startup/shutdown.
6. Add unit tests for event matching, dispatch, and deduplication.

### Phase 3: CLI and API Integration
7. Add `--notify` flag to `cmd/start.go`.
8. Add `NotifySubscribers` field to `CreateAgentRequest` in `pkg/hubclient/agents.go`.
9. Update `createAgent` handler to create subscriptions from request.
10. Add `ScopeAgentNotify` scope and auto-grant alongside creation scopes.
11. Error messaging for local-mode `--notify` usage.

### Phase 4: Testing and Polish
12. Integration tests: agent-creates-agent-with-notify flow.
13. Status normalization edge cases.
14. Subscription cleanup on agent deletion verification.

### Future Phases
- REST API for subscription management (`GET/POST/DELETE /subscriptions`).
- `scion get-notified <agent>` CLI command.
- Human user notification sinks (email, Slack, web push).
- Stale/stalled detection.
- Message retry/queuing for offline subscribers.

---

## Files Affected (Initial Implementation)

| File | Change |
|---|---|
| `pkg/store/models.go` | Add `NotificationSubscription` model |
| `pkg/store/store.go` | Add `NotificationSubscriptionStore` interface |
| `pkg/store/sqlite/sqlite.go` | New migration, interface implementation |
| `pkg/hub/notifications.go` | **New** — `NotificationDispatcher` |
| `pkg/hub/notifications_test.go` | **New** — Unit tests |
| `pkg/hub/server.go` | Wire dispatcher into startup/shutdown |
| `pkg/hub/handlers.go` | Create subscriptions in `createAgent` |
| `pkg/hub/agenttoken.go` | Add `ScopeAgentNotify` |
| `pkg/hubclient/agents.go` | Add `NotifySubscribers` to `CreateAgentRequest` |
| `cmd/start.go` | Add `--notify` flag |
| `cmd/common.go` | Pass notify subscribers to Hub request |

---

## Related Documents

- [Agent-to-Hub Access](agent-hub-access.md) — Agent JWT scopes and sub-agent creation.
- [Web Realtime Events](web-realtime.md) — `ChannelEventPublisher` design and SSE.
- [Hub Messaging](hub-messaging.md) — CLI message routing through Hub.
- [Hub API](hub-api.md) — REST API specification.
- [Hosted Architecture](hosted-architecture.md) — System overview.
