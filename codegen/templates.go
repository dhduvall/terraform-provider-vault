package codegen

import (
	"fmt"
	"io"
	"io/ioutil"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"github.com/hashicorp/errwrap"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/vault/sdk/framework"
)

var (
	// templateRegistry holds templates for each type of file.
	templateRegistry = map[templateType]string{
		// TODO in separate PR - add templateTypeDataSource
		// TODO in separate PR - add templateTypeDoc
		templateTypeResource: "/codegen/templates/resource.go.tpl",
	}

	// These are the types of fields that OpenAPI 3 has that we support
	// converting into Terraform fields.
	supportedParamTypes = []string{
		"array",
		"boolean",
		"integer",
		"string",
	}
)

func newTemplateHandler(logger hclog.Logger) (*templateHandler, error) {
	homeDirPath, err := pathToHomeDir()
	if err != nil {
		return nil, err
	}

	// Read in the template for each template type in the registry and
	// cache them to be used repeatedly.
	templates := make(map[templateType]*template.Template, len(templateRegistry))
	for tmplType, pathFromHomeDir := range templateRegistry {
		pathToFile := filepath.Join(homeDirPath, pathFromHomeDir)
		templateBytes, err := ioutil.ReadFile(pathToFile)
		if err != nil {
			return nil, errwrap.Wrapf("error reading "+pathToFile+": {{err}}", err)
		}
		t, err := template.New(tmplType.String()).Parse(string(templateBytes))
		if err != nil {
			return nil, errwrap.Wrapf("error parsing "+tmplType.String()+": {{err}}", err)
		}
		templates[tmplType] = t
	}
	return &templateHandler{
		logger:               logger,
		templates:            templates,
		templatableEndpoints: make(map[string]*templatableEndpoint),
	}, nil
}

type templateHandler struct {
	logger               hclog.Logger
	templates            map[templateType]*template.Template
	templatableEndpoints map[string]*templatableEndpoint
}

// Write takes one endpoint and uses a template to generate text
// for it. This template is written to the given writer.
func (h *templateHandler) Write(wr io.Writer, tmplType templateType, endpoint string, endpointInfo *framework.OASPathItem) error {
	templatable, ok := h.templatableEndpoints[endpoint]
	if !ok {
		// Since each endpoint will have a code file and a doc file, let's cache
		// the template-friendly version of the endpoint so it doesn't have to be
		// converted into that format twice.
		var err error
		templatable, err = h.toTemplatable(endpoint, endpointInfo)
		if err != nil {
			return err
		}
		h.templatableEndpoints[endpoint] = templatable
	}
	return h.templates[tmplType].Execute(wr, templatable)
}

// toTemplatable converts the provided data into a struct that the template
// can easily interpret.
func (h *templateHandler) toTemplatable(endpoint string, endpointInfo *framework.OASPathItem) (*templatableEndpoint, error) {
	parameters := collectParameters(endpointInfo)

	// Sort the parameters by name so they won't shift every time
	// new files are generated due to having originated in maps.
	sort.Slice(parameters, func(i, j int) bool {
		return parameters[i].Name < parameters[j].Name
	})

	// De-duplicate the parameters in place, because often parameters
	// are at both the top-level and in the post body. This in-place
	// approach is directly recommended here:
	// https://github.com/golang/go/wiki/SliceTricks#in-place-deduplicate-comparable
	j := 0
	for i := 1; i < len(parameters); i++ {
		if parameters[j] == parameters[i] {
			continue
		}
		j++
		// preserve the original data
		// in[i], in[j] = in[j], in[i]
		// only set what is required
		parameters[j] = parameters[i]
	}
	parameters = parameters[:j+1]

	// The last field in the endpoint will be something like "name"
	// or "roles" or whatever is at the end of an endpoint's path.
	// This is used to differentiate generated variable or function names
	// so they don't collide with the other ones in the same package.
	tmplName := clean(path.Base(endpoint))
	t := &templatableEndpoint{
		Endpoint:                endpoint,
		DirName:                 clean(path.Base(filepath.Dir(endpoint))),
		UpperCaseDifferentiator: strings.Title(strings.ToLower(tmplName)),
		LowerCaseDifferentiator: strings.ToLower(tmplName),
		Parameters:              parameters,
		SupportsRead:            endpointInfo.Get != nil,
		SupportsWrite:           endpointInfo.Post != nil,
		SupportsDelete:          endpointInfo.Delete != nil,
	}
	if err := t.Validate(); err != nil {
		return nil, errwrap.Wrapf("failed to validate templatable data for "+endpoint+": {{err}}", err)
	}
	return t, nil
}

// collectParameters walks a PathItem and looks for all the parameters
// described. Some are at the top level of the path, indicating they are
// path parameters. Others are only in the post body.
func collectParameters(endpointInfo *framework.OASPathItem) []*templatableParam {
	var result []*templatableParam
	for _, param := range endpointInfo.Parameters {
		result = append(result, toTemplatableParam(param, true))
	}
	if endpointInfo.Post == nil || endpointInfo.Post.RequestBody == nil || endpointInfo.Post.RequestBody.Content == nil {
		return result
	}
	for _, mediaTypeObject := range endpointInfo.Post.RequestBody.Content {
		if mediaTypeObject.Schema == nil || mediaTypeObject.Schema.Properties == nil {
			continue
		}
		for paramName, schema := range mediaTypeObject.Schema.Properties {
			param := framework.OASParameter{
				Name:        paramName,
				Description: schema.Description,
				In:          "post",
				Schema:      schema,
			}
			result = append(result, toTemplatableParam(param, false))
		}
	}
	return result
}

// templatableParam mainly just reuses the OASParameter,
// but adds on a IsPathParam bool.
type templatableParam struct {
	*framework.OASParameter
	IsPathParam bool
}

func toTemplatableParam(param framework.OASParameter, isPathParameter bool) *templatableParam {
	ptrToParam := &param
	if ptrToParam.Schema == nil {
		// Always populate schema and display attributes so later it'll be easier
		// to check if they're sensitive by iterating over them.
		ptrToParam.Schema = &framework.OASSchema{}
	}
	if ptrToParam.Schema.DisplayAttrs == nil {
		ptrToParam.Schema.DisplayAttrs = &framework.DisplayAttributes{}
	}
	return &templatableParam{
		OASParameter: ptrToParam,
		IsPathParam:  isPathParameter,
	}
}

// templatableEndpoint is a convenience struct that plays nicely with Go's
// template package. It is used to keep as much logic as possible in Go
// rather than in Go's templating language, because most folks are more
// familiar with Go.
type templatableEndpoint struct {
	Endpoint                string
	DirName                 string
	UpperCaseDifferentiator string
	LowerCaseDifferentiator string
	Parameters              []*templatableParam
	SupportsRead            bool
	SupportsWrite           bool
	SupportsDelete          bool
}

func (e *templatableEndpoint) Validate() error {
	if e == nil {
		return fmt.Errorf("endpoint is nil")
	}
	var errs error
	if e.Endpoint == "" {
		errs = multierror.Append(errs, fmt.Errorf("endpoint cannot be blank for %#v", e))
	}
	if e.DirName == "" {
		errs = multierror.Append(errs, fmt.Errorf("dirname cannot be blank for %#v", e))
	}
	if e.UpperCaseDifferentiator == "" {
		errs = multierror.Append(errs, fmt.Errorf("exported function prefix cannot be blank for %#v", e))
	}
	if e.LowerCaseDifferentiator == "" {
		errs = multierror.Append(errs, fmt.Errorf("private function prefix cannot be blank for %#v", e))
	}
	for _, parameter := range e.Parameters {
		if err := validateParameter(parameter); err != nil {
			errs = multierror.Append(errs, fmt.Errorf("error validating "+parameter.Name+": {{err}}", err))
		}
	}
	return errs
}

func validateParameter(parameter *templatableParam) error {
	for _, supportedType := range supportedParamTypes {
		if parameter.Schema.Type == supportedType {
			if parameter.Schema.Type != "array" {
				// We have a match, and if the type isn't an array, we don't
				// need to look into its element types to see if they're
				// supported.
				return nil
			}
			if parameter.Schema.Items.Type == "string" {
				// Right now, our templates assume that all array types are strings.
				// If we allow other types of arrays, we will need to also go into
				// each template and add additional logic supporting the new array
				// type.
				return nil
			}
			return fmt.Errorf("unsupported array type of %s for %s", parameter.Schema.Items.Type, parameter.Name)
		}
	}
	return fmt.Errorf("unsupported parameter type of %s for %s", parameter.Schema.Type, parameter.Name)
}

// clean takes a field like "{role_name}" and returns
// "rolename" for use in generated Go code.
func clean(field string) string {
	field = stripCurlyBraces(field)
	return strings.Replace(field, "_", "", -1)
}

type templateType int

const (
	templateTypeUnset templateType = iota
	templateTypeDataSource
	templateTypeResource
	templateTypeDoc
)

func (t templateType) String() string {
	switch t {
	case templateTypeDataSource:
		return "datasources"
	case templateTypeResource:
		return "resources"
	case templateTypeDoc:
		return "docs"
	}
	return "unset"
}
