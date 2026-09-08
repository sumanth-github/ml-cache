import pandas as pd
from sklearn.ensemble import RandomForestClassifier
from sklearn.model_selection import train_test_split
from sklearn.metrics import classification_report
import joblib

df = pd.read_csv('training_data.csv')
X = df[['freq', 'last_access_gap', 'size_bytes','ttl_remaining', 'cache_used']]
y = df['evict_label']

X_train, X_test, y_train, y_test = train_test_split(
    X, y, test_size=0.2, random_state=42, stratify=y
)

model = RandomForestClassifier(n_estimators=100, max_depth=10, random_state=42, class_weight='balanced')
model.fit(X_train, y_train)

print(classification_report(y_test, model.predict(X_test)))
print("feature_importances:", dict(zip(X.columns, model.feature_importances_)))

joblib.dump(model, 'eviction_model.pkl')
print("saved eviction_model.pkl")