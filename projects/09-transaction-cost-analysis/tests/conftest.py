from __future__ import annotations

import pytest
from builders import FIXTURE_DIR, WORKED_DIR

from tca.attribution import Analysis
from tca.service import TcaService


@pytest.fixture
def service() -> TcaService:
    return TcaService()


@pytest.fixture(scope="session")
def fixture_analysis() -> Analysis:
    """The simulated fixture dataset, imported and analysed once per test session."""
    service = TcaService()
    for result in service.import_directory(FIXTURE_DIR):
        assert result.rejected == 0
    return service.analysis()


@pytest.fixture
def worked_service() -> TcaService:
    service = TcaService()
    service.import_directory(WORKED_DIR)
    return service
