//! Exact decimal-string ⇄ integer-unit conversion (prices to ticks, sizes to
//! lots). Never goes through floating point.

#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum DecimalError {
    #[error("`{0}` is not a plain non-negative decimal number")]
    Malformed(String),
    #[error("`{value}` is not a whole multiple of the unit `{unit}`")]
    NotMultiple { value: String, unit: String },
    #[error("`{0}` is out of range")]
    OutOfRange(String),
    #[error("unit `{0}` must be greater than zero")]
    ZeroUnit(String),
}

const MAX_LEN: usize = 30;

/// Parses `"123.450"` into `(mantissa, scale)` = `(123450, 3)`.
fn parse(s: &str) -> Result<(i128, u32), DecimalError> {
    let bad = || DecimalError::Malformed(s.to_string());
    if s.is_empty() || s.len() > MAX_LEN {
        return Err(bad());
    }
    let (int, frac) = s.split_once('.').unwrap_or((s, ""));
    if int.is_empty() && frac.is_empty() {
        return Err(bad());
    }
    if !int.bytes().chain(frac.bytes()).all(|b| b.is_ascii_digit()) {
        return Err(bad());
    }
    let digits = format!("{int}{frac}");
    let mantissa: i128 = if digits.is_empty() {
        0
    } else {
        digits.parse().map_err(|_| bad())?
    };
    Ok((mantissa, frac.len() as u32))
}

/// Converts `value` into a whole number of `unit`s, e.g.
/// `to_units("100.25", "0.01") == Ok(10025)`.
pub fn to_units(value: &str, unit: &str) -> Result<i64, DecimalError> {
    let (vm, vs) = parse(value)?;
    let (um, us) = parse(unit)?;
    if um == 0 {
        return Err(DecimalError::ZeroUnit(unit.to_string()));
    }
    let scale = vs.max(us);
    let range = || DecimalError::OutOfRange(value.to_string());
    let lift = |m: i128, s: u32| {
        10i128
            .checked_pow(scale - s)
            .and_then(|p| m.checked_mul(p))
            .ok_or_else(range)
    };
    let (v, u) = (lift(vm, vs)?, lift(um, us)?);
    if v % u != 0 {
        return Err(DecimalError::NotMultiple {
            value: value.to_string(),
            unit: unit.to_string(),
        });
    }
    i64::try_from(v / u).map_err(|_| range())
}

/// Inverse of [`to_units`]: `format_units(10025, "0.01") == "100.25"`.
/// Keeps the unit's number of decimal places.
pub fn format_units(units: i64, unit: &str) -> Result<String, DecimalError> {
    let (um, us) = parse(unit)?;
    if units < 0 {
        return Err(DecimalError::OutOfRange(units.to_string()));
    }
    let m = i128::from(units)
        .checked_mul(um)
        .ok_or_else(|| DecimalError::OutOfRange(units.to_string()))?;
    if us == 0 {
        return Ok(m.to_string());
    }
    let pow = 10i128.pow(us);
    Ok(format!(
        "{}.{:0width$}",
        m / pow,
        m % pow,
        width = us as usize
    ))
}
