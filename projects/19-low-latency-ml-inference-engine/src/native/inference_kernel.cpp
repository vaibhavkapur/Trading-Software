#include "inference_kernel.hpp"

#include <algorithm>
#include <cmath>
#include <cstdint>
#include <new>
#include <vector>

namespace {

constexpr int32_t kMaxStackDim = 256;

struct DenseLayer {
    int32_t in_dim = 0;
    int32_t out_dim = 0;
    int32_t activation = ML_ACT_LINEAR;
    std::size_t weights_offset = 0;
    std::size_t biases_offset = 0;
};

inline double apply_activation(double x, int32_t act) noexcept {
    switch (act) {
        case ML_ACT_LINEAR:
            return x;
        case ML_ACT_RELU:
            return x > 0.0 ? x : 0.0;
        case ML_ACT_TANH:
            return std::tanh(x);
        case ML_ACT_SIGMOID: {
            if (x >= 0.0) {
                const double z = std::exp(-x);
                return 1.0 / (1.0 + z);
            }
            const double z = std::exp(x);
            return z / (1.0 + z);
        }
        default:
            return x;
    }
}

}  // namespace

struct MlNativeModel {
    int32_t num_features = 0;
    int32_t max_dim = 0;
    std::vector<double> feat_mean;
    std::vector<double> feat_std;
    std::vector<double> feat_min;
    std::vector<double> feat_max;
    std::vector<double> feat_clip_z;
    std::vector<DenseLayer> layers;
    std::vector<double> flat_params;
};

namespace {

inline int32_t score_single_with_buffers(
    const MlNativeModel& model,
    const double* raw_features,
    double* buf_a,
    double* buf_b,
    double* out_prediction
) noexcept {
    const int32_t nf = model.num_features;
    const double* means = model.feat_mean.data();
    const double* stds = model.feat_std.data();
    const double* mins = model.feat_min.data();
    const double* maxs = model.feat_max.data();
    const double* clips = model.feat_clip_z.data();

    for (int32_t i = 0; i < nf; ++i) {
        const double x = raw_features[i];
        if (!std::isfinite(x)) {
            return ML_ENGINE_ERR_NON_FINITE_INPUT;
        }
        if (x < mins[i] || x > maxs[i]) {
            return ML_ENGINE_ERR_OUT_OF_BOUNDS;
        }
        double z = (x - means[i]) / stds[i];
        const double c = clips[i];
        if (z > c) {
            z = c;
        } else if (z < -c) {
            z = -c;
        }
        buf_a[i] = z;
    }

    double* cur = buf_a;
    double* nxt = buf_b;
    const double* params = model.flat_params.data();

    for (const DenseLayer& layer : model.layers) {
        const int32_t in_d = layer.in_dim;
        const int32_t out_d = layer.out_dim;
        const int32_t act = layer.activation;
        const double* w_base = params + layer.weights_offset;
        const double* b_base = params + layer.biases_offset;

        for (int32_t j = 0; j < out_d; ++j) {
            const double* w_row = w_base + static_cast<std::size_t>(j) * static_cast<std::size_t>(in_d);
            double acc = b_base[j];
            for (int32_t i = 0; i < in_d; ++i) {
                acc += w_row[i] * cur[i];
            }
            nxt[j] = apply_activation(acc, act);
        }
        std::swap(cur, nxt);
    }

    const double result = cur[0];
    if (!std::isfinite(result)) {
        return ML_ENGINE_ERR_NON_FINITE_OUTPUT;
    }
    *out_prediction = result;
    return ML_ENGINE_OK;
}

}  // namespace

extern "C" {

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
) {
    if (num_features <= 0 || num_layers <= 0 || total_weight_doubles <= 0) {
        return nullptr;
    }
    if (!feat_mean || !feat_std || !feat_min || !feat_max || !feat_clip_z ||
        !layer_in_dims || !layer_out_dims || !layer_activations || !flat_weights) {
        return nullptr;
    }

    auto* model = new (std::nothrow) MlNativeModel();
    if (!model) {
        return nullptr;
    }

    model->num_features = num_features;
    model->max_dim = num_features;
    model->feat_mean.assign(feat_mean, feat_mean + num_features);
    model->feat_std.assign(feat_std, feat_std + num_features);
    model->feat_min.assign(feat_min, feat_min + num_features);
    model->feat_max.assign(feat_max, feat_max + num_features);
    model->feat_clip_z.assign(feat_clip_z, feat_clip_z + num_features);

    for (int32_t i = 0; i < num_features; ++i) {
        if (!std::isfinite(model->feat_mean[i]) || !std::isfinite(model->feat_std[i]) ||
            model->feat_std[i] <= 0.0 || !std::isfinite(model->feat_min[i]) ||
            !std::isfinite(model->feat_max[i]) || model->feat_min[i] > model->feat_max[i] ||
            !std::isfinite(model->feat_clip_z[i]) || model->feat_clip_z[i] <= 0.0) {
            delete model;
            return nullptr;
        }
    }

    int32_t prev_dim = num_features;
    std::size_t offset = 0;
    model->layers.reserve(static_cast<std::size_t>(num_layers));
    for (int32_t l = 0; l < num_layers; ++l) {
        const int32_t in_d = layer_in_dims[l];
        const int32_t out_d = layer_out_dims[l];
        const int32_t act = layer_activations[l];
        if (in_d != prev_dim || out_d <= 0 || act < ML_ACT_LINEAR || act > ML_ACT_SIGMOID) {
            delete model;
            return nullptr;
        }
        DenseLayer layer;
        layer.in_dim = in_d;
        layer.out_dim = out_d;
        layer.activation = act;
        layer.weights_offset = offset;
        offset += static_cast<std::size_t>(in_d) * static_cast<std::size_t>(out_d);
        layer.biases_offset = offset;
        offset += static_cast<std::size_t>(out_d);
        model->layers.push_back(layer);
        model->max_dim = std::max(model->max_dim, out_d);
        prev_dim = out_d;
    }

    if (prev_dim != 1 || static_cast<int64_t>(offset) != total_weight_doubles) {
        delete model;
        return nullptr;
    }

    model->flat_params.assign(flat_weights, flat_weights + total_weight_doubles);
    for (double w : model->flat_params) {
        if (!std::isfinite(w)) {
            delete model;
            return nullptr;
        }
    }
    return model;
}

void ml_engine_destroy_model(MlNativeModel* model) {
    delete model;
}

int32_t ml_engine_score_one(
    const MlNativeModel* model,
    const double* raw_features,
    int32_t num_features,
    double* out_prediction
) {
    if (!model || !raw_features || !out_prediction) {
        return ML_ENGINE_ERR_NULL_ARG;
    }
    if (num_features != model->num_features) {
        return ML_ENGINE_ERR_INVALID_MODEL;
    }

    if (model->max_dim <= kMaxStackDim) {
        alignas(64) double buf_a[kMaxStackDim];
        alignas(64) double buf_b[kMaxStackDim];
        return score_single_with_buffers(*model, raw_features, buf_a, buf_b, out_prediction);
    }

    thread_local std::vector<double> tl_a;
    thread_local std::vector<double> tl_b;
    if (static_cast<int32_t>(tl_a.size()) < model->max_dim) {
        tl_a.resize(static_cast<std::size_t>(model->max_dim));
        tl_b.resize(static_cast<std::size_t>(model->max_dim));
    }
    return score_single_with_buffers(*model, raw_features, tl_a.data(), tl_b.data(), out_prediction);
}

int32_t ml_engine_score_batch(
    const MlNativeModel* model,
    const double* raw_features_batch,
    int32_t batch_size,
    int32_t num_features,
    double* out_predictions
) {
    if (!model || !raw_features_batch || !out_predictions) {
        return ML_ENGINE_ERR_NULL_ARG;
    }
    if (batch_size <= 0) {
        return ML_ENGINE_ERR_INVALID_BATCH;
    }
    if (num_features != model->num_features) {
        return ML_ENGINE_ERR_INVALID_MODEL;
    }

    if (model->max_dim <= kMaxStackDim) {
        alignas(64) double buf_a[kMaxStackDim];
        alignas(64) double buf_b[kMaxStackDim];
        for (int32_t b = 0; b < batch_size; ++b) {
            const double* row = raw_features_batch + static_cast<std::size_t>(b) * static_cast<std::size_t>(num_features);
            const int32_t st = score_single_with_buffers(*model, row, buf_a, buf_b, &out_predictions[b]);
            if (st != ML_ENGINE_OK) {
                return st;
            }
        }
        return ML_ENGINE_OK;
    }

    thread_local std::vector<double> tl_a;
    thread_local std::vector<double> tl_b;
    if (static_cast<int32_t>(tl_a.size()) < model->max_dim) {
        tl_a.resize(static_cast<std::size_t>(model->max_dim));
        tl_b.resize(static_cast<std::size_t>(model->max_dim));
    }
    for (int32_t b = 0; b < batch_size; ++b) {
        const double* row = raw_features_batch + static_cast<std::size_t>(b) * static_cast<std::size_t>(num_features);
        const int32_t st = score_single_with_buffers(*model, row, tl_a.data(), tl_b.data(), &out_predictions[b]);
        if (st != ML_ENGINE_OK) {
            return st;
        }
    }
    return ML_ENGINE_OK;
}

const char* ml_engine_version(void) {
    return "cpp20-native-kernel-1.0.0";
}

}  // extern "C"
