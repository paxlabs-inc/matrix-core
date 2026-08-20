# Performance Optimization

Performance best practices for React Native, React, and cross-platform development.

## Critical Performance Rules

### 1. Concurrent Request Control

**Problem**: Too many simultaneous network requests can block the UI thread via React Native bridge saturation.

**Rule**: Always limit concurrent network requests to prevent bridge message queue overflow.

```typescript
// ❌ BAD - All requests fire simultaneously
const requests = items.map(item => fetchData(item));
await Promise.all(requests); // Can cause UI freeze with 15+ requests

// ✅ GOOD - Batched execution with concurrency limit
async function executeBatched<T>(
  tasks: Array<() => Promise<T>>,
  concurrency = 3,
): Promise<Array<PromiseSettledResult<T>>> {
  const results: Array<PromiseSettledResult<T>> = [];

  for (let i = 0; i < tasks.length; i += concurrency) {
    const batch = tasks.slice(i, i + concurrency);
    const batchResults = await Promise.allSettled(
      batch.map((task) => task()),
    );
    results.push(...batchResults);
  }

  return results;
}

// Usage
const tasks = items.map(item => () => fetchData(item));
const results = await executeBatched(tasks, 3); // Max 3 concurrent
```

**Why this matters**:
- Each network request generates 6+ React Native bridge messages
- 15 concurrent requests = 90+ bridge messages = UI freeze
- iPhone 7 and older devices are especially vulnerable
- iOS Watchdog kills app after 5s freeze (AppHang)

**When to use**:
- Loading data for multiple networks/accounts
- Batch operations (bulk updates, sync operations)
- Any scenario with 5+ potential concurrent requests

### 2. React Native Bridge Optimization

**Problem**: The React Native bridge is serialized - only one message crosses at a time. High bridge traffic delays UI updates.

**Rules**:

#### Minimize Bridge Crossings

```typescript
// ❌ BAD - Multiple bridge crossings
items.forEach(item => {
  NativeModules.MyModule.update(item.id, item.value); // Each call = bridge crossing
});

// ✅ GOOD - Single bridge crossing
NativeModules.MyModule.batchUpdate(items); // Pass all data at once
```

#### Avoid Large Data Transfers

```typescript
// ❌ BAD - Transferring large objects
const hugeData = await fetchLargeDataset(); // 10MB JSON
setState(hugeData); // Serialization cost + bridge crossing

// ✅ GOOD - Paginate or lazy load
const page1 = await fetchPage(1, 50); // 100KB chunks
setState(page1);
// Load more on demand
```

#### Debounce Frequent Updates

```typescript
// ❌ BAD - Every keystroke crosses bridge
<TextInput
  onChangeText={(text) => {
    NativeModules.Analytics.trackInput(text); // Bridge spam
  }}
/>

// ✅ GOOD - Debounced updates
import { debounce } from 'lodash';

const trackInput = debounce((text) => {
  NativeModules.Analytics.trackInput(text);
}, 500);

<TextInput onChangeText={trackInput} />
```

### 3. Main Thread Protection

**Problem**: JavaScript operations can block the main thread, causing UI jank and freezes.

**Rules**:

#### Defer Heavy Operations

```typescript
// ❌ BAD - Heavy work during render/navigation
function MyScreen() {
  const data = processLargeDataset(); // Blocks render
  return <View>{data}</View>;
}

// ✅ GOOD - Defer to next tick
function MyScreen() {
  const [data, setData] = useState(null);

  useEffect(() => {
    // Let UI render first
    setTimeout(() => {
      setData(processLargeDataset());
    }, 0);
  }, []);

  if (!data) return <Spinner />;
  return <View>{data}</View>;
}

// ✅ BETTER - Use InteractionManager (React Native)
import { InteractionManager } from 'react-native';

useEffect(() => {
  const task = InteractionManager.runAfterInteractions(() => {
    setData(processLargeDataset());
  });
  return () => task.cancel();
}, []);
```

#### Avoid Synchronous Heavy Computation

```typescript
// ❌ BAD - Synchronous heavy work
function calculateStats(items: Item[]) {
  // Heavy computation in loop
  return items.map(item => ({
    ...item,
    score: complexAlgorithm(item), // Blocking
  }));
}

// ✅ GOOD - Break into chunks with yielding
async function calculateStats(items: Item[]) {
  const results = [];
  const CHUNK_SIZE = 100;

  for (let i = 0; i < items.length; i += CHUNK_SIZE) {
    const chunk = items.slice(i, i + CHUNK_SIZE);
    results.push(...chunk.map(item => ({
      ...item,
      score: complexAlgorithm(item),
    })));

    // Yield to event loop every chunk
    await new Promise(resolve => setTimeout(resolve, 0));
  }

  return results;
}
```

### 4. React Component Optimization

#### Avoid Expensive Operations in Render

```typescript
// ❌ BAD - Expensive operation every render
function TokenList({ tokens }: { tokens: Token[] }) {
  const sorted = tokens.sort((a, b) =>
    expensiveComparison(a, b) // Runs every render!
  );
  return <List data={sorted} />;
}

// ✅ GOOD - Memoize expensive operations
function TokenList({ tokens }: { tokens: Token[] }) {
  const sorted = useMemo(
    () => tokens.sort((a, b) => expensiveComparison(a, b)),
    [tokens],
  );
  return <List data={sorted} />;
}
```

#### Use memo for Heavy Components

```typescript
// ❌ BAD - Re-renders even when props unchanged
function HeavyComponent({ data }: { data: Data }) {
  // Expensive rendering logic
  return <ComplexUI data={data} />;
}

// ✅ GOOD - Skip re-render if props unchanged
import { memo } from 'react';

const HeavyComponent = memo(function HeavyComponent({ data }: { data: Data }) {
  return <ComplexUI data={data} />;
});
```

#### Stable Callbacks

```typescript
// ❌ BAD - New function every render
function Parent() {
  return <Child onPress={() => handlePress()} />; // New ref every time
}

// ✅ GOOD - Stable callback reference
function Parent() {
  const handlePress = useCallback(() => {
    // handler logic
  }, []); // Stable reference

  return <Child onPress={handlePress} />;
}
```

### 5. List Rendering Optimization

#### Use FlashList for Long Lists (React Native)

```typescript
// ❌ BAD - FlatList for 1000+ items
import { FlatList } from 'react-native';

<FlatList
  data={thousandsOfItems}
  renderItem={({ item }) => <Item data={item} />}
/>

// ✅ GOOD - FlashList for better performance
import { FlashList } from '@shopify/flash-list';

<FlashList
  data={thousandsOfItems}
  renderItem={({ item }) => <Item data={item} />}
  estimatedItemSize={80} // Important for performance
/>
```

#### Optimize windowSize for Memory Management (React Native)

**Problem**: Default `windowSize` (21) keeps too many items mounted, consuming memory on low-end devices.

**Solution**: Reduce `windowSize` to limit number of rendered items outside viewport.

```typescript
// ❌ BAD - Default windowSize = 21 (renders 21 pages of items)
<FlatList
  data={items}
  renderItem={renderItem}
  // windowSize not specified - defaults to 21
/>

// ✅ GOOD - Reduced windowSize for better memory usage
<FlatList
  data={items}
  renderItem={renderItem}
  windowSize={5} // Renders 5 pages (2 above + current + 2 below)
/>

// ✅ BETTER - Conditional based on platform and context
<FlatList
  data={items}
  renderItem={renderItem}
  windowSize={platformEnv.isNativeAndroid && inTabList ? 3 : 5}
  // Android in tabs: 3 (memory constrained)
  // Other platforms: 5 (balanced)
/>
```

**How windowSize works**:
- `windowSize={5}` means: 5 × viewport height of items are mounted
- Example: If viewport shows 10 items:
  - windowSize=5 → 50 items mounted (20 above + 10 visible + 20 below)
  - windowSize=3 → 30 items mounted (10 above + 10 visible + 10 below)
  - windowSize=21 (default) → 210 items mounted

**When to use smaller windowSize**:
- Low-end devices (especially Android)
- Complex item components (heavy rendering)
- Lists inside tabs/nested scrollviews
- Memory-constrained scenarios

**Trade-offs**:
- ✅ Lower memory usage
- ✅ Better performance on low-end devices
- ❌ More frequent item mounting/unmounting during fast scrolling
- ❌ Potential blank space if scrolling very fast

---

**🚨 Developer Alert: windowSize Optimization**

**✅ Using an optimized `ListView` component?**
- ✅ **Already optimized with `windowSize={5}` built-in**
- ✅ **No need to set windowSize manually**
- ⚠️ **ONLY override if you need a different value** (e.g., `windowSize={3}` for Android tabs)

**⚠️ Using `FlatList` or custom list component?**
- ⚠️ **Set `windowSize={3-5}` manually** (default 21 is too large)
- Recommended: `windowSize={5}` (balanced)
- Memory-constrained: `windowSize={3}` (Android tabs, low-end devices)

---

**Example using optimized ListView**:

```typescript
// ✅ RECOMMENDED - Use an optimized ListView (already optimized)
import { ListView } from '@{scope}/components';

<ListView
  data={items}
  renderItem={renderItem}
  estimatedItemSize={80}
  // windowSize={5} is already set internally - no need to specify!
/>

// ⚠️ ONLY set windowSize when you need different value
<ListView
  data={items}
  renderItem={renderItem}
  estimatedItemSize={80}
  windowSize={3} // Override only for specific cases (e.g., Android tabs)
/>
```

#### Virtualization Keys

```typescript
// ❌ BAD - Index as key (causes re-renders)
{items.map((item, index) => (
  <Item key={index} data={item} />
))}

// ✅ GOOD - Stable unique key
{items.map((item) => (
  <Item key={item.id} data={item} />
))}
```

#### Item Component Memoization

```typescript
// ❌ BAD - List item re-renders on parent update
function ListItem({ item }: { item: Item }) {
  return <View>{item.name}</View>;
}

// ✅ GOOD - Memoized list item
const ListItem = memo(function ListItem({ item }: { item: Item }) {
  return <View>{item.name}</View>;
});
```

#### CSS content-visibility for Long Lists (Web Only)

**Problem**: Browser renders all DOM elements even if they're off-screen, causing slow initial render.

**Solution**: Use `content-visibility: auto` to defer off-screen rendering.

```css
/* Apply to list items */
.message-item {
  content-visibility: auto;
  contain-intrinsic-size: 0 80px; /* Estimated item height for layout calculations */
}
```

**Example**:

```tsx
// Web component with CSS optimization
function MessageList({ messages }: { messages: Message[] }) {
  return (
    <div className="overflow-y-auto h-screen">
      {messages.map(msg => (
        <div key={msg.id} className="message-item">
          <Avatar user={msg.author} />
          <div>{msg.content}</div>
        </div>
      ))}
    </div>
  );
}
```

**Performance Impact**:
- For 1000 messages: Browser skips layout/paint for ~990 off-screen items
- **10× faster initial render** on long lists
- Rendering work happens lazily as user scrolls

**When to use**:
- Web applications with long scrollable lists
- Item height is relatively uniform
- 100+ items in the list
- Initial render performance is critical

**Important Notes**:
- ✅ This is a **CSS-only** optimization for web platforms
- ✅ For React Native, use `ListView` or `FlashList` instead
- ✅ Browser support: Chrome/Edge 85+, Safari 16.4+

---

### 🚨 Developer Alert: Built-in Optimizations

**✅ Already Optimized - NO ACTION NEEDED:**

| Component | Optimization | What's Included |
|-----------|--------------|-----------------|
| `ListView` from component library | `windowSize={5}` | Automatically limits visible items to 5× viewport height |
| `Tabs` from component library | `contentVisibility: 'hidden'` | Inactive tabs auto-hidden, only focused tab visible |
| `Dialog` from component library | `contentVisibility: 'hidden'` | Auto-hidden when closed (with `forceMount`) |
| Modal Navigators | `contentVisibility: 'hidden'` | Non-current routes auto-hidden |

**⚠️ MANUAL ACTION REQUIRED - Business Components:**

| Scenario | Action | Example |
|----------|--------|---------|
| Long scrollable lists (100+ items) | Add `content-visibility: auto` CSS to list items | Message list, transaction history, gallery |
| Custom tab-like components | Add `contentVisibility: 'hidden'` manually | Not using component library Tabs |
| Custom visibility toggles | Add `contentVisibility: 'hidden'` manually | Collapsible panels, accordion items, show/hide sections |
| FlatList with heavy items | Set `windowSize={3-5}` manually | Product list, image gallery with complex items |

**Quick Decision Tree:**
```
Are you using base components from your component library?
├─ YES (ListView, Tabs, Dialog) → ✅ Already optimized, do nothing
└─ NO (Custom/business components)
   └─ Is it a long list (100+ items)?
      ├─ YES → ⚠️ Add content-visibility: auto (Web) or windowSize (Native)
      └─ NO → Is it tab-like or toggle-able content?
         ├─ YES → ⚠️ Add contentVisibility: 'hidden'
         └─ NO → No action needed
```

---

**Components with Built-in `contentVisibility`**:

The following components **can use** `contentVisibility: 'hidden'` to optimize inactive content:

1. **`Tabs` Component**:
   - Automatically hides inactive tab content
   - Uses `contentVisibility: 'hidden'` for non-focused tabs
   - ✅ **No need to add contentVisibility when using Tabs**

   ```typescript
   // ✅ Tabs component auto-optimizes with contentVisibility
   import { Tabs } from '@{scope}/components';

   <Tabs>
     <Tabs.Tab name="tab1" label="Tab 1">
       <HeavyContent /> {/* Auto-hidden when not focused */}
     </Tabs.Tab>
     <Tabs.Tab name="tab2" label="Tab 2">
       <AnotherHeavyContent /> {/* Auto-hidden when not focused */}
     </Tabs.Tab>
   </Tabs>
   // Internal: element.style.contentVisibility = isFocused ? 'visible' : 'hidden'
   ```

2. **Modal Navigators**:
   - Hides non-current routes
   - ✅ **Automatically optimized**

   ```typescript
   // Internal implementation for non-current routes:
   // style={{ contentVisibility: !isCurrentRoute ? 'hidden' : undefined }}
   ```

3. **`Dialog` Component**:
   - Uses `contentVisibility: 'hidden'` when closed with `forceMount`
   - ✅ **Automatically optimized**

   ```typescript
   // ✅ Dialog auto-hides when closed but force-mounted
   <Dialog forceMount>
     <Dialog.Content>
       {/* Hidden with contentVisibility when dialog is closed */}
     </Dialog.Content>
   </Dialog>
   ```

**When to manually add `contentVisibility`**:
- ⚠️ **Long scrollable lists** (100+ items) - use `content-visibility: auto` for list items
- ⚠️ **Custom tab-like components** - not using component library Tabs
- ⚠️ **Custom visibility toggles** - manually shown/hidden content

**How to apply for long lists**:
1. Create a CSS class with `content-visibility: auto`
2. Apply the class to your list item components
3. Set appropriate `contain-intrinsic-size` (estimated item height)

### 6. State Updates Optimization

#### Batch State Updates

```typescript
// ❌ BAD - Multiple state updates = multiple re-renders
function updateAll() {
  setName('John');     // Re-render 1
  setAge(30);          // Re-render 2
  setEmail('j@a.com'); // Re-render 3
}

// ✅ GOOD - React 18+ auto-batches in event handlers
function updateAll() {
  setName('John');
  setAge(30);
  setEmail('j@a.com');
  // Single re-render (React 18+)
}

// ✅ GOOD - Use single state object for related data
const [user, setUser] = useState({ name: '', age: 0, email: '' });
function updateAll() {
  setUser({ name: 'John', age: 30, email: 'j@a.com' });
  // Single re-render
}
```

#### Avoid Derived State

```typescript
// ❌ BAD - Duplicating state
function UserProfile({ user }: { user: User }) {
  const [name, setName] = useState(user.name); // Duplicate!

  useEffect(() => {
    setName(user.name); // Sync nightmare
  }, [user.name]);
}

// ✅ GOOD - Use props directly or derive during render
function UserProfile({ user }: { user: User }) {
  const displayName = user.name.toUpperCase(); // Derive on render
  return <Text>{displayName}</Text>;
}
```

### 7. Image Optimization

```typescript
// ❌ BAD - No size constraints
<Image source={{ uri: imageUrl }} />

// ✅ GOOD - Specify dimensions
<Image
  source={{ uri: imageUrl }}
  style={{ width: 100, height: 100 }}
  resizeMode="cover"
/>

// ✅ BETTER - Use optimized image component
import { Image } from '@{scope}/components';

<Image
  src={imageUrl}
  width={100}
  height={100}
  // Auto-optimizes and caches
/>
```

### 8. Async Operation Patterns

#### Parallel vs Sequential

```typescript
// ❌ BAD - Sequential when parallel would work
const user = await fetchUser();
const settings = await fetchSettings();
const preferences = await fetchPreferences();
// Total: 300ms + 200ms + 150ms = 650ms

// ✅ GOOD - Parallel independent requests
const [user, settings, preferences] = await Promise.all([
  fetchUser(),
  fetchSettings(),
  fetchPreferences(),
]);
// Total: max(300ms, 200ms, 150ms) = 300ms

// ⚠️ IMPORTANT - But limit concurrency for many requests!
// See Rule #1: Concurrent Request Control
```

#### Cancellation for Stale Requests

```typescript
// ❌ BAD - Race condition with stale data
function SearchInput() {
  const [query, setQuery] = useState('');
  const [results, setResults] = useState([]);

  useEffect(() => {
    search(query).then(setResults); // Old results may arrive late!
  }, [query]);
}

// ✅ GOOD - Cancel stale requests
function SearchInput() {
  const [query, setQuery] = useState('');
  const [results, setResults] = useState([]);

  useEffect(() => {
    const controller = new AbortController();

    search(query, { signal: controller.signal })
      .then(setResults)
      .catch(err => {
        if (err.name !== 'AbortError') throw err;
      });

    return () => controller.abort(); // Cancel on cleanup
  }, [query]);
}
```

## Performance Measurement

### React DevTools Profiler

```typescript
// Wrap components to measure performance
import { Profiler } from 'react';

<Profiler
  id="MyComponent"
  onRender={(id, phase, actualDuration) => {
    if (actualDuration > 16) { // Over 1 frame (60fps)
      console.warn(`${id} slow render: ${actualDuration}ms`);
    }
  }}
>
  <MyComponent />
</Profiler>
```

### React Native Performance Monitor

```typescript
// Enable in dev mode - use a performance monitoring utility
import { PerformanceMonitor } from '@{scope}/shared/src/perf';

// Monitor specific operations
const measure = PerformanceMonitor.start('list-load');
await loadData();
measure.end(); // Logs if > threshold
```

## Performance Checklist

Before merging performance-critical code, verify:

- [ ] **Network requests**: Limited to 3-5 concurrent
- [ ] **Bridge crossings**: Minimized, data batched
- [ ] **Heavy operations**: Deferred with `InteractionManager` or `setTimeout`
- [ ] **List rendering**: Using `FlashList` for 100+ items (React Native) or `content-visibility` for long lists (Web)
- [ ] **Component memoization**: Heavy components wrapped with `memo`
- [ ] **Callbacks**: Stable with `useCallback` when passed to memoized children
- [ ] **Expensive computations**: Memoized with `useMemo`
- [ ] **State updates**: Batched, no derived state
- [ ] **Images**: Sized appropriately, using optimized component
- [ ] **Async operations**: Cancellable, avoid race conditions

## Real-World Example: iOS AppHang Fix

**Problem**: 15+ concurrent network requests caused 5-second UI freeze on iPhone 7

**Root Cause**:
```
15 concurrent requests
  → 90+ React Native bridge messages
  → Bridge saturation
  → Main thread blocked
  → iOS Watchdog kills app (AppHang)
```

**Solution**: Batched execution with concurrency limit

```typescript
// Before: All requests fire at once
const requests = accountAddressList.map(account =>
  this.updateNetworkTokenList(account)
);
await Promise.all(requests); // 💥 15+ concurrent

// After: Batched with limit of 3
const tasks = accountAddressList.map(account =>
  () => this.updateNetworkTokenList(account)
);
const results = await this.executeBatched(tasks, 3); // ✅ Max 3 concurrent
```

**Result**:
- UI freeze eliminated
- Smooth navigation animations
- No more AppHang errors
- Better error handling with `Promise.allSettled`

**Lesson**: Always consider device capabilities and bridge limitations when designing concurrent operations.

## Performance Anti-Patterns

### 1. "It works on my device"

```typescript
// ❌ Your MacBook Pro can handle this, iPhone 7 cannot
await Promise.all(twentyRequests); // Works on M1, hangs on A10
```

**Solution**: Test on low-end devices (iPhone 7, Android mid-range)

### 2. "Premature optimization"

```typescript
// ❌ Over-memoizing simple components
const Button = memo(function Button({ label }: { label: string }) {
  return <Text>{label}</Text>; // Too simple to benefit from memo
});
```

**Solution**: Profile first, optimize bottlenecks

### 3. "Memo everything"

```typescript
// ❌ Memoizing where it hurts performance
const expensiveMemoCheck = useMemo(
  () => cheapOperation(),
  [dep1, dep2, dep3, dep4, dep5], // Expensive dependency check!
);
```

**Solution**: Only memoize expensive operations (>10ms)

## Related Documentation

- [Promise Handling](./promise-handling.md) - Async patterns and error handling
- [React Components](./react-components.md) - Component structure and hooks
- [Error Handling](./error-handling.md) - Error boundaries and recovery

## External References

- [React Native Performance](https://reactnative.dev/docs/performance)
- [React Profiler API](https://react.dev/reference/react/Profiler)
- [FlashList Documentation](https://shopify.github.io/flash-list/)
