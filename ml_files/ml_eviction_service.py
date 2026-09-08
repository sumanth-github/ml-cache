# Fixed ml_eviction_service.py with web interface
from fastapi import FastAPI, HTTPException, Response
from fastapi.responses import HTMLResponse
from pydantic import BaseModel
from typing import Dict, List, Any
from prometheus_client import Counter, Histogram, generate_latest, CONTENT_TYPE_LATEST
import pandas as pd
import joblib
import logging
import json
import os
import time

# Prometheus metrics
ml_predictions_total = Counter('ml_predictions_total', 'Total ML predictions made')
ml_prediction_duration = Histogram('ml_prediction_duration_seconds', 'Time spent on ML predictions')

# Setup logging
logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)

app = FastAPI(
    title="ML Cache Eviction Service",
    description="Machine Learning service for intelligent cache eviction decisions",
    version="1.0.0"
)

# Load your trained model
model = None
model_paths = [
    'eviction_model.pkl',
    'ml_files/eviction_model.pkl',
    './ml_files/eviction_model.pkl',
    os.path.join(os.getcwd(), 'ml_files', 'eviction_model.pkl')
]

for path in model_paths:
    try:
        if os.path.exists(path):
            model = joblib.load(path)
            logger.info(f"Model loaded successfully from: {path}")
            
            if hasattr(model, 'feature_names_in_'):
                logger.info(f"Model expects features: {list(model.feature_names_in_)}")
            else:
                logger.info("Model feature names not available")
            break
    except Exception as e:
        logger.warning(f"Failed to load model from {path}: {e}")
        continue

if model is None:
    logger.error("Failed to load model from any path")
    logger.info(f"Current working directory: {os.getcwd()}")
    logger.info(f"Files in current directory: {os.listdir('.')}")
    if os.path.exists('ml_files'):
        logger.info(f"Files in ml_files/: {os.listdir('ml_files')}")

class CacheState(BaseModel):
    keys: List[str]
    features: Dict[str, Any]

# ROOT ENDPOINT - This fixes the "Not Found" error
@app.get("/", response_class=HTMLResponse)
async def root():
    """Root endpoint with service information and links"""
    
    model_status = "✅ Loaded" if model else "❌ Not Loaded"
    model_type = type(model).__name__ if model else "None"
    status_class = "healthy" if model else "unhealthy"
    
    # Get prediction count safely
    try:
        prediction_count = ml_predictions_total._value._value if hasattr(ml_predictions_total._value, '_value') else 0
    except:
        prediction_count = 0
    
    feature_count = '5' if model and hasattr(model, 'feature_names_in_') else '0'
    ml_status = 'Active' if model else 'Inactive'
    
    # Use string concatenation to avoid format conflicts
    html_content = f"""
<!DOCTYPE html>
<html>
<head>
    <title>ML Cache Eviction Service</title>
    <style>
        body {{ font-family: Arial, sans-serif; margin: 40px; background-color: #f5f5f5; }}
        .container {{ max-width: 800px; background: white; padding: 30px; border-radius: 10px; box-shadow: 0 2px 10px rgba(0,0,0,0.1); }}
        .header {{ color: #333; border-bottom: 2px solid #007acc; padding-bottom: 10px; }}
        .status {{ padding: 15px; margin: 15px 0; border-radius: 5px; }}
        .status.healthy {{ background-color: #d4edda; color: #155724; border: 1px solid #c3e6cb; }}
        .status.unhealthy {{ background-color: #f8d7da; color: #721c24; border: 1px solid #f5c6cb; }}
        .endpoints {{ margin: 20px 0; }}
        .endpoint {{ margin: 10px 0; padding: 10px; background-color: #f8f9fa; border-left: 4px solid #007acc; }}
        .endpoint a {{ color: #007acc; text-decoration: none; font-weight: bold; }}
        .endpoint a:hover {{ text-decoration: underline; }}
        .stats {{ display: flex; justify-content: space-around; margin: 20px 0; }}
        .stat {{ text-align: center; padding: 15px; background-color: #e9ecef; border-radius: 5px; }}
        .stat-number {{ font-size: 24px; font-weight: bold; color: #007acc; }}
        .stat-label {{ font-size: 14px; color: #666; }}
    </style>
</head>
<body>
    <div class="container">
        <h1 class="header">🤖 ML Cache Eviction Service</h1>
        
        <div class="status {status_class}">
            <h3>Service Status</h3>
            <p><strong>Model Status:</strong> {model_status}</p>
            <p><strong>Model Type:</strong> {model_type}</p>
            <p><strong>Service:</strong> Running on http://localhost:8000</p>
        </div>
        
        <div class="stats">
            <div class="stat">
                <div class="stat-number">{prediction_count}</div>
                <div class="stat-label">ML Predictions</div>
            </div>
            <div class="stat">
                <div class="stat-number">{feature_count}</div>
                <div class="stat-label">Features Expected</div>
            </div>
            <div class="stat">
                <div class="stat-number">{ml_status}</div>
                <div class="stat-label">ML Status</div>
            </div>
        </div>
        
        <div class="endpoints">
            <h3>🔗 Available Endpoints</h3>
            
            <div class="endpoint">
                <a href="/health">/health</a>
                <p>Service health check and status information</p>
            </div>
            
            <div class="endpoint">
                <a href="/debug/features">/debug/features</a>
                <p>View expected ML model features and configuration</p>
            </div>
            
            <div class="endpoint">
                <a href="/metrics">/metrics</a>
                <p>Prometheus metrics for monitoring</p>
            </div>
            
            <div class="endpoint">
                <a href="/docs">/docs</a>
                <p>Interactive API documentation (Swagger UI)</p>
            </div>
            
            <div class="endpoint">
                <strong>POST /predict</strong>
                <p>Main ML eviction prediction endpoint (requires JSON payload)</p>
            </div>
        </div>
        
        <div class="status healthy">
            <h3>📝 Usage Example</h3>
            <p><strong>Test the prediction endpoint:</strong></p>
            <pre style="background: #f8f9fa; padding: 10px; border-radius: 3px; overflow-x: auto;">
curl -X POST http://localhost:8000/predict \\
  -H "Content-Type: application/json" \\
  -d '{{
    "keys": ["key1", "key2", "key3"],
    "features": {{
      "key1": {{"freq": 5, "last_access": 1000, "size_bytes": 100, "ttl_remaining": 0, "cache_used": 1000}},
      "key2": {{"freq": 2, "last_access": 500, "size_bytes": 200, "ttl_remaining": 0, "cache_used": 1000}},
      "key3": {{"freq": 1, "last_access": 200, "size_bytes": 150, "ttl_remaining": 0, "cache_used": 1000}}
    }}
  }}'
</pre>
        </div>
    </div>
</body>
</html>
"""
    
    return html_content



@app.post("/predict")
async def predict_eviction(request: CacheState):
    with ml_prediction_duration.time():
        ml_predictions_total.inc()

    logger.info(f"=== ML PREDICTION REQUEST ===")
    logger.info(f"Number of keys: {len(request.keys)}")
    
    if len(request.keys) == 0:
        logger.warning("No keys provided")
        raise HTTPException(status_code=400, detail="No keys provided")
    
    if not model:
        logger.error("Model not loaded - falling back to simple heuristic")
        return {
            "evict_key": request.keys[0], 
            "fallback": True,
            "error": "Model not available"
        }
    
    try:
        # Debug: Log the first few features to understand structure
        sample_keys = list(request.features.keys())[:3]
        logger.info(f"Sample keys: {sample_keys}")
        
        for key in sample_keys:
            logger.info(f"Features for {key}: {request.features[key]}")
        
        # Convert features to DataFrame
        features_data = []
        
        for key in request.keys:
            if key in request.features:
                key_features = request.features[key]
                
                if isinstance(key_features, dict):
                    # Map Go feature names to model expected names
                    feature_mapping = {
                        'frequency': 'freq',
                        'freq': 'freq',
                        'last_access': 'last_access',
                        'size': 'size_bytes',
                        'size_bytes': 'size_bytes',
                        'ttl_remaining': 'ttl_remaining',
                        'cache_used': 'cache_used'
                    }
                    
                    # Set default values
                    feature_row = {
                        'freq': 0,
                        'last_access': 0,
                        'size_bytes': 0,
                        'ttl_remaining': 0.0,
                        'cache_used': 0
                    }

                    now_ts = time.time()
                    feature_row['last_access_gap'] =max(now_ts - feature_row['last_access'],0 ) if feature_row['last_access'] else 0
                    del feature_row['last_access']
                    # Update with actual values
                    for go_key, model_key in feature_mapping.items():
                        if go_key in key_features:
                            try:
                                feature_row[model_key] = float(key_features[go_key])
                            except (ValueError, TypeError):
                                logger.warning(f"Could not convert {go_key}={key_features[go_key]} to float")
                                pass
                    
                    features_data.append(feature_row)
                else:
                    logger.warning(f"Unexpected feature structure for key {key}: {key_features}")
            else:
                logger.warning(f"No features found for key: {key}")
        
        if not features_data:
            logger.error("No valid features found - using fallback")
            return {"evict_key": request.keys[0], "fallback": True}
        
        # Create DataFrame with expected columns
        expected_cols = ['freq', 'last_access_gap', 'size_bytes', 'ttl_remaining', 'cache_used']
        df = pd.DataFrame(features_data, index=request.keys)
        
        # Ensure all expected columns exist
        for col in expected_cols:
            if col not in df.columns:
                df[col] = 0.0
                
        # Reorder columns to match model expectations
        df = df[expected_cols]
        
        logger.info(f"DataFrame shape: {df.shape}")
        logger.info(f"DataFrame sample:\n{df.head()}")
        
        # Fill any remaining NaN values
        df = df.fillna(0)
        
        # Make prediction
        try:
            predictions = model.predict_proba(df)
            logger.info(f"Predictions shape: {predictions.shape}")
            
            # Get the class with highest eviction probability
            if predictions.shape[1] > 1:
                eviction_scores = predictions[:, 1]  # Probability of class 1 (should evict)
            else:
                eviction_scores = predictions[:, 0]
                
            best_idx = eviction_scores.argmax()
            evict_key = request.keys[best_idx]
            eviction_prob = eviction_scores[best_idx]
            
            logger.info(f"Selected key: {evict_key} with probability: {eviction_prob:.4f}")
            
            return {
                "evict_key": evict_key,
                "probability": float(eviction_prob),
                "debug_info": {
                    "total_keys": len(request.keys),
                    "predictions_shape": predictions.shape,
                    "max_prob": float(eviction_prob)
                }
            }
            
        except Exception as pred_error:
            logger.error(f"Prediction error: {pred_error}")
            logger.error(f"DataFrame dtypes: {df.dtypes}")
            
            # Fallback: return least frequent key (simple heuristic)
            if 'freq' in df.columns and not df['freq'].isna().all():
                least_freq_key = df['freq'].idxmin()
                logger.info(f"Fallback: selecting least frequent key: {least_freq_key}")
                return {"evict_key": least_freq_key, "fallback": True}
            else:
                # Return first key as last resort
                return {"evict_key": request.keys[0], "fallback": True}
                
    except Exception as e:
        logger.error(f"Error processing request: {e}")
        logger.error(f"Request keys: {request.keys}")
        logger.error(f"Features keys: {list(request.features.keys())}")
        
        # Always return something to prevent 500 error
        return {
            "evict_key": request.keys[0] if request.keys else "unknown", 
            "fallback": True, 
            "error": str(e)
        }

@app.get("/health")
async def health_check():
    return {
        "status": "healthy",
        "model_loaded": model is not None,
        "model_type": type(model).__name__ if model else None,
        "cwd": os.getcwd(),
        "model_paths_checked": model_paths,
        "total_predictions": ml_predictions_total._value._value if hasattr(ml_predictions_total._value, '_value') else 0
    }

@app.get("/debug/features")
async def debug_features():
    """Debug endpoint to check expected model features"""
    if not model:
        return {"error": "Model not loaded"}
    
    info = {"model_type": type(model).__name__}
    
    if hasattr(model, 'feature_names_in_'):
        info["expected_features"] = list(model.feature_names_in_)
    if hasattr(model, 'n_features_in_'):
        info["expected_feature_count"] = model.n_features_in_
        
    return info

@app.get("/metrics")
async def get_metrics():
    return Response(generate_latest(), media_type=CONTENT_TYPE_LATEST)

if __name__ == "__main__":
    import uvicorn
    uvicorn.run(app, host="0.0.0.0", port=8000)
