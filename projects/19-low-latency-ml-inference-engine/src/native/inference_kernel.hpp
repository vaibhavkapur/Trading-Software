#pragma once

#include <cstddef>
#include <cstdint>

#ifdef __cplusplus
extern "C" {
#endif

// C ABI error codes returned by the native C++20 inference kernel.
enum MlEngineStatus : int32_t {
    ML_ENGINE_OK = 0,
    ML_ENGINE_ERR_NULL_ARG = -1,
    ML_ENGINE_ERR_NON_FINITE_INPUT = -2,
    ML_ENGINE_ERR_OUT_OF_BOUNDS = -3,
    ML_ENGINE_ERR_NON_FINITE_OUTPUT = -4,
    ML_ENGINE_ERR_INVALID_MODEL = -5,
    ML_ENGINE_ERR_INVALID_BATCH = -6
};

// Activation codes matching contracts.py ALLOWED_ACTIVATIONS.
enum MlActivationCode : int32_t {
    ML_ACT_LINEAR = 0,
    ML_ACT_RELU = 1,
    ML_ACT_TANH = 2,
    ML_ACT_SIGMOID = 3
};

struct MlNativeModel;

// Create an immutable native model handle from verified preprocessing & layer arrays.
// Returns nullptr if parameters are invalid or non-finite.
MlNativeModel* ml_engine_create_model(
    int32_t num_features,
    const double* feat_mean,
    const double* feat_std,
    const double* feat_min,
    const double* feat_max,
    const double* feat_clip_z,
    int32_t num_layers,
    const int32_t* layer_in_dims,
    const int32_t* layer_out_dims,
    const int32_t* layer_activations,
    const double* flat_weights,
    int64_t total_weight_doubles
);

void ml_engine_destroy_model(MlNativeModel* model);

// Zero-allocation hot-path single-vector inference (preprocess + MLP forward pass).
// Thread-safe: model is immutable and scratch memory is stack-allocated.
int32_t ml_engine_score_one(
    const MlNativeModel* model,
    const double* raw_features,
    int32_t num_features,
    double* out_prediction
);

// Zero-allocation hot-path batch inference over row-major [batch_size x num_features].
// Thread-safe.
int32_t ml_engine_score_batch(
    const MlNativeModel* model,
    const double* raw_features_batch,
    int32_t batch_size,
    int32_t num_features,
    double* out_predictions
);

const char* ml_engine_version(void);

#ifdef __cplusplus
}
#endif
