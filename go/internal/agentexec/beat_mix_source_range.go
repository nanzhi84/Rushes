package agentexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

type beatMixSourceRange struct {
	StartFrame     int
	EndFrame       int
	QualityPenalty float64
}
