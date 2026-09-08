import pandas as pd
import numpy as np

df = pd.read_csv('../access_log.csv')
df = df.reset_index(drop=True)
df['row_id'] = df.index

# next_access_row[key] = row_id of this row's next GET for the same key (or +inf)
next_get_row = {}
df['next_access_delta'] = np.inf  # rows-until-next-GET, our reuse distance proxy

# walk backwards: for each row, if it's a GET, record row_id as "next access" for that key
# then look up what the *previous* row's next_access should point to
last_get_row = {}
next_dist = np.full(len(df), np.inf)

for i in range(len(df) - 1, -1, -1):
    key = df.at[i, 'key']
    op = df.at[i, 'operation']
    if key in last_get_row:
        next_dist[i] = last_get_row[key] - i
    if op == 'GET':
        last_get_row[key] = i

df['reuse_distance'] = next_dist

# state tracking for freq/last_access/size at time of each row (causal features only)
freq = {}
last_access = {}
size_map = {}

feat_rows = []
for i, row in df.iterrows():
    key, op = row['key'], row['operation']
    if op == 'SET':
        size_map[key] = row['size_bytes']
        freq[key] = freq.get(key, 0)  # SET doesn't bump freq; GET does
        last_access[key] = row['timestamp']
    elif op == 'GET' and row['hit'] == 1:
        freq[key] = freq.get(key, 0) + 1
        prev_last_access = last_access.get(key, row['timestamp'])
        feat_rows.append({
            'key': key,
            'freq': freq[key],
            'last_access_gap': row['timestamp'] - prev_last_access,  # causal: time since prior access
            'size_bytes': size_map.get(key, 0),
            'ttl_remaining' : row['ttl_remaining'],
            'cache_used': row['cache_used'],
            'reuse_distance': row['reuse_distance'],
        })
        last_access[key] = row['timestamp']

train_df = pd.DataFrame(feat_rows)
# label: bottom 30% by reuse_distance (soon-to-be-reused) = keep (0); rest = evict-candidate (1)
finite = train_df[np.isfinite(train_df['reuse_distance'])]
threshold = finite['reuse_distance'].quantile(0.7) if len(finite) > 0 else np.inf
train_df['evict_label'] = (
    (train_df['reuse_distance'] > threshold) | (~np.isfinite(train_df['reuse_distance']))
).astype(int)
cap = finite['reuse_distance'].max()*2 if len(finite) else 0
train_df['reuse_distance'] = train_df['reuse_distance'].replace(np.inf, cap)
train_df.to_csv('training_data.csv', index=False)
print(f"Built {len(train_df)} labeled rows, {train_df['evict_label'].mean()*100:.1f}% positive")