var crypto = require('crypto');
var util = require('util');
var response = require('./cfn-response');
var _ = require('./underscore-min');
var async = require('./async.min');
var AWS = require('aws-sdk');
var awsConfig = new AWS.Config({});
//awsConfig.logger = console;

console.log('NodeJS v.' + process.version + ', AWS SDK v.' + AWS.VERSION);
var apigateway = new AWS.APIGateway(awsConfig);
var lambda = new AWS.Lambda(awsConfig);

var RE_STATEMENT_ALREADY_EXISTS = /ResourceConflictException.*already exists/;

// TODO - provide function that returns a list of all lambda functions
// defined in this stack definition

////////////////////////////////////////////////////////////////////////////////
// UTILITY FUNCTIONS
var logResults = function(msgText, e, results) {
  var msg = {
    ERROR: e || undefined,
    RESULTS: results || undefined
  };
  console.log(util.format('%s =>\n%s', msgText, JSON.stringify(msg, null, ' ')));
};

var statementID = function(lambdaArn) {
  var shasum = crypto.createHash('sha1');
  shasum.update(lambdaArn);
  return util.format('Sparta%s', shasum.digest('hex'));
};

var lamdbdaURI = function(lambdaArn) {
  return util.format('arn:aws:apigateway:%s:lambda:path/2015-03-31/functions/%s/invocations',
    lambda.config.region,
    lambdaArn);
};

var accumulatedStackLambdas = function(resourcesRoot, accumulator) {
  // If this is the API root node, then be a bit flexible
  accumulator = accumulator || [];

  var apiChildren = resourcesRoot.APIResources || {};
  _.each(apiChildren, function(eachValue /*, eachKey */ ) {
    if (eachValue.LambdaArn) {
      accumulator.push(eachValue.LambdaArn);
    }
  });
  var children = resourcesRoot.Children || {};
  _.each(children, function(eachValue /*, eachKey */ ) {
    accumulatedStackLambdas(eachValue, accumulator);
  });
  return accumulator;
};

////////////////////////////////////////////////////////////////////////////////
// BEGIN - DELETE API FUNCTIONS
var ensureLambdaPermissionsDeleted = function(lambdaFunctionArns, callback) {
  var cleanupIterator = function(eachArn, iterCB) {
    var onCleanup = function(  e, result  ) {
      logResults('removePermission result', null, {
        ERROR: e,
        RESULTS: result,
        ARM: eachArn
      });
      iterCB(null, null);
    };
    try
    {
      var params = {
        FunctionName: eachArn,
        StatementId: statementID(eachArn)
      };
      lambda.removePermission(params, onCleanup);
    }
    catch (e)
    {
      logResults('Failed to remove permission', e, null);
      setImmediate(onCleanup, e, {});
    }
  };
  async.eachSeries(lambdaFunctionArns, cleanupIterator, callback);
};

var ensureAPIDeletedTask = function(resourceProperties, oldResourceProperties /*, returnData */) {
  return function task(callback /*, results*/ ) {
    var waterfall = [];
    // Get all the APIs
    waterfall.push(function(cb) {
      apigateway.getRestApis({}, cb);
    });

    waterfall.push(function(restAPIs, cb) {
      var apiProps = resourceProperties.API || {};
      var matchingAPI = _.find(restAPIs.items || [], function(eachRestAPI) {
        return eachRestAPI.name === apiProps.Name;
      });

      // After the API is deleted, give a best effort attempt to
      // cleanup the permissions
      var onAPIDeleted = function(e, results) {
        if (!e) {
          var oldAPIProps = oldResourceProperties.API || {};
          var lambdaArns = accumulatedStackLambdas(oldAPIProps.Resources || []);
          ensureLambdaPermissionsDeleted(lambdaArns, cb);
        } else {
          cb(e, results);
        }
      };
      // If the API exists, find it by name and delete it
      if (matchingAPI) {
        logResults('Deleting API', null, matchingAPI);
        var params = {
          restApiId: matchingAPI.id
        };
        apigateway.deleteRestApi(params, onAPIDeleted);
      } else {
        setImmediate(onAPIDeleted, null, true);
      }
    });
    var terminus = function( /* e, results */ ) {
      callback(null, true);
    };
    async.waterfall(waterfall, terminus);
  };
};
// END - DELETE API FUNCTIONS
////////////////////////////////////////////////////////////////////////////////

////////////////////////////////////////////////////////////////////////////////
// BEGIN - CREATE API FUNCTIONS
var ensureAPICreatedTask = function(resourceProperties, returnData) {
  return function task(callback /*, results */ ) {
    var apiProps = resourceProperties.API || {};
    var params = {
      name: apiProps.Name,
      cloneFrom: apiProps.CloneFrom || undefined,
      description: apiProps.Description || undefined
    };
    var terminus = function(e, createResults) {
      returnData.ensureAPICreated = {
        Error: e ? e.toString() : undefined,
        Results: createResults ? createResults : undefined
      };
      setImmediate(callback, null, createResults);
    };
    apigateway.createRestApi(params, terminus);
  };
};

var ensureLambdaPermissionCreated = function(lambdaArn, resourceMethodDefinition, rolePolicyCache, callback) {
  var addPermissionParams = {
    Action: 'lambda:InvokeFunction',
    FunctionName: lambdaArn,
    Principal: 'apigateway.amazonaws.com',
    StatementId: statementID(lambdaArn),
  };
  var cachedValues = rolePolicyCache[lambdaArn] || {};
  var matching = _.find(Object.keys(cachedValues), function(eachKey) {
    return cachedValues[eachKey].Sid === addPermissionParams.StatementId;
  });
  if (matching) {
    setImmediate(callback, null, {});
  } else {
    // Add it and cache it...
    var creationTasks = {};
    creationTasks.add = function(asyncCB) {
      var onAddPermission = function(e, result) {
        if (e && RE_STATEMENT_ALREADY_EXISTS.test(e.toString())) {
          logResults('Statement already exists', null, e.toString());
          e = null;
        }
        asyncCB(e, result);
      };
      lambda.addPermission(addPermissionParams, onAddPermission);
    };
    creationTasks.cache = ['add'];
    creationTasks.cache.push(function(asyncCB) {
      var getPolicyParams = {
        FunctionName: lambdaArn
      };
      lambda.getPolicy(getPolicyParams, asyncCB);
    });
    var terminus = function(e, results) {
      if (!e && results.cache) {
        try {
          rolePolicyCache[lambdaArn] = JSON.parse(results.cache.Policy);
          logResults('Cached IAM Role', null, {ARN: lambdaArn, POLICY: rolePolicyCache[lambdaArn]});
        } catch (eParse) {
          e = eParse;
        }
      }
      callback(e, results);
    };
    async.auto(creationTasks, terminus);
  }
};

var ensureAPIResourceMethodsCreated = function(restApiId, awsResourceId, APIDefinition, rolePolicyCache, createdCB) {
  // Iterator applied to each member of the methodOpParams// object
  var methodCreationIterator = function(lambdaArn, methodName, methodDef, cb) {
    var creationTasks = {};
    // Parameters common to all Method-related API calls
    var methodOpParams = function(apiSpecificParams) {
      return _.extend({
        httpMethod: methodDef.HTTPMethod,
        resourceId: awsResourceId,
        restApiId: restApiId,
      }, apiSpecificParams || {});
    };

    // 1. Create the Method entry
    // Create the method
    var apiKeyRequired = (typeof(methodDef.APIKeyRequired) === 'boolean') ?
                          methodDef.APIKeyRequired :
                          ('true' === methodDef.APIKeyRequired);

    creationTasks.putMethod = function(asyncCB) {
      var params = methodOpParams({
        authorizationType: methodDef.AuthorizationType || "NONE",
        apiKeyRequired: apiKeyRequired,
        requestParameters: {}
      });
      apigateway.putMethod(params, asyncCB);
    };

    // 2. Create the Method response
    // Create the method responses
    creationTasks.putMethodResponse = Object.keys(creationTasks);
    creationTasks.putMethodResponse.push(function(asyncCB) {
      var params = methodOpParams({
        statusCode: '200',
        responseModels: {
          'application/json': 'Empty'
        }
      });
      apigateway.putMethodResponse(params, asyncCB);
    });

    // 3. Create the Method integration
    // Create the method integration
    creationTasks.putIntegration = Object.keys(creationTasks);
    creationTasks.putIntegration.push(function(asyncCB) {
      var params = methodOpParams({
        type: 'AWS',
        cacheKeyParameters: [],
        uri: lamdbdaURI(lambdaArn),
        integrationHttpMethod: 'POST'
      });
      apigateway.putIntegration(params, asyncCB);
    });

    // 4. Create the integration response
    // The integration responses
    creationTasks.putIntegrationResponse = Object.keys(creationTasks);
    creationTasks.putIntegrationResponse.push(function(asyncCB) {
      var params = methodOpParams({
        statusCode: '200',
        responseTemplates: {
          'application/json': '',
        }
      });
      apigateway.putIntegrationResponse(params, asyncCB);
    });

    // 5. Punch a hole into the Lambda s.t. this Arn has permission to invoke the function
    // Related: https://forums.aws.amazon.com/message.jspa?messageID=678324
    creationTasks.addPermission = Object.keys(creationTasks);
    creationTasks.addPermission.push(function(asyncCB /*, context */ ) {
      try
      {
        ensureLambdaPermissionCreated(lambdaArn, methodDef, rolePolicyCache, asyncCB);
      }
      catch (e)
      {
        logResults('Failed to addPermission', e, methodDef);
        setImmediate(asyncCB, e, null);
      }
    });

    // TODO: remove logging code
    // When we're done, describe everything to see what it looks like
    creationTasks.methodDescription = Object.keys(creationTasks);
    creationTasks.methodDescription.push(function(asyncCB) {
      apigateway.getMethod(methodOpParams({}), asyncCB);
    });

    // Wrap it up
    var terminus = function(e, createResults) {
      logResults('methodCreationIterator results', e, createResults);
      cb(e, createResults);
    };
    async.auto(creationTasks, terminus);
  };

  // Start the iteration, which requires the Lambda ARN
  // Create the HTTP methods for this item.
  var lambdaArn = APIDefinition.LambdaArn;
  var methods = Object.keys(APIDefinition.Methods);
  async.eachSeries(methods, function(eachMethod, seriesCB) {
    methodCreationIterator(lambdaArn, eachMethod, APIDefinition.Methods[eachMethod], seriesCB);
  }, createdCB);
};

var ensureResourcesCreatedTask = function(restAPIKeyName, resourceProperties /*, returnData */ ) {
  return function task(callback, results) {
    var apiCreatedResults = results[restAPIKeyName] || {};
    var restApiId = apiCreatedResults.id || "";

    var tasks = [];
    // Get the current resources
    tasks.push(function(cb) {
      var params = {
        restApiId: restApiId,
        limit: "100",
      };
      apigateway.getResources(params, cb);
    });

    // Turn them into a {path, resourceID} map
    tasks.push(function(getResults, cb) {
      var resourceIndex = {};
      if (getResults && getResults.items) {
        resourceIndex = _.reduce(getResults.items,
          function(memo, eachItem) {
            memo[eachItem.path] = eachItem.id;
            return memo;
          }, {});
      }
      setImmediate(cb, null, resourceIndex);
    });

    // Create all the resources in the custom data
    tasks.push(function(resourceIndex, taskCB) {
      logResults('Resource Index', null, resourceIndex);
      var lambdaRolePolicyCache = {};

      var workerError = null;

      //////////////////////////////////////////////////////////////////////////
      // The queue worker for resources visited as the
      // visitor descends the "API" property
      var processResourceEntry = function(taskData, processCB) {
        var rootObject = taskData.definition;
        var parentResourceId = taskData.parentId;

        ////////////////////////////////////////////////////////////////////////
        var onProcessComplete = function(e, processTaskResults) {
          workerError = e;
          if (e) {
            console.log('ERROR: ' + e.toString());
          }
          if (!workerError) {
            // Push the parent ID into the child
            var children = rootObject.Children || {};
            var childKeys = Object.keys(children);
            childKeys.forEach(function(eachKey) {
              var task = {
                definition: children[eachKey],
                parentId: processTaskResults.createResource.id
              };
              logResults('Pushing child task', null, eachKey);
              workerQueue.push(task);
            });
          }
          processCB(workerError);
        };

        ////////////////////////////////////////////////////////////////////////
        // Make sure the PathComponent is already in the resourceIndex
        var processTasks = {};
        processTasks.createResource = function(asyncCB) {
          // If there is a parentId, then create the child resource
          // for this path
          if (parentResourceId) {
            // Create the resource...
            var params = {
              parentId: parentResourceId,
              pathPart: rootObject.PathComponent,
              restApiId: restApiId
            };
            apigateway.createResource(params, asyncCB);
          } else {
            logResults('Resource already exists', null, {
              PATH: rootObject.PathComponent
            });
            // No need to create a child resource for "/" path
            setImmediate(asyncCB, null, {
              id: resourceIndex["/"]
            });
          }
        };

        ////////////////////////////////////////////////////////////////////////
        // Create the Methods
        processTasks.createMethods = ['createResource'];
        processTasks.createMethods.push(function(asyncCB, context) {
          var createResourceResponse = context.createResource || {};
          logResults('CONTEXT', null, context);
          logResults('createResource response', null, createResourceResponse);

          // The API resources will be created a of the root resource
          // or the previously created resource id subpath component
          var resourceId = createResourceResponse.id || resourceIndex["/"];
          var apiResources = rootObject.APIResources || {};
          var apiKeys = Object.keys(apiResources);
          var onAPIResourcesComplete = function(e /*, results */ ) {
            asyncCB(e, e ? null : resourceId);
          };
          async.eachSeries(apiKeys, function(eachKey, itorCB) {
            ensureAPIResourceMethodsCreated(restApiId, resourceId, apiResources[eachKey], lambdaRolePolicyCache, itorCB);
          }, onAPIResourcesComplete);
        });
        async.auto(processTasks, onProcessComplete);
      };

      // Setup the queue to descend
      var workerQueue = async.queue(processResourceEntry, 1);
      workerQueue.drain = function() {
        taskCB(workerError, true);
      };
      var apiDefinition = resourceProperties.API || {};
      var rootResourceDefinition = apiDefinition.Resources || {};
      workerQueue.push({
        definition: rootResourceDefinition,
        parentId: null
      });
    });
    async.waterfall(tasks, callback);
  };
};

var ensureDeploymentTask = function(restAPIKeyName, resourceProperties /*, returnData */ ) {
  return function task(callback, context) {
   var apiCreatedResults = context[restAPIKeyName] || {};
   var restApiId = apiCreatedResults.id || "";

   var apiDefinition = resourceProperties.API || {};
   var stageDefinition = apiDefinition.Stage || {};
   if (stageDefinition.Name)
   {
     var deployTasks = [];
     deployTasks.push(function (taskCB) {
       var params = {
         restApiId: restApiId,
         stageName: stageDefinition.Name,
         cacheClusterEnabled: ("true" === stageDefinition.CacheClusterEnabled),
         cacheClusterSize: _.isEmpty(stageDefinition.CacheClusterSize) ? undefined : stageDefinition.CacheClusterSize,
         stageDescription: stageDefinition.Description || '',
         variables: stageDefinition.Variables || {}
       };
       logResults('Creating deployment', null, params);
       apigateway.createDeployment(params, taskCB);
     });
    async.waterfall(deployTasks, callback);
   }
   else
   {
     // No stage requested
     logResults('Stage not requested', null, restApiId);
     setImmediate(callback, null, restApiId);
   }
  };
};

exports.handler = function(event, context) {
  var data = {};
  console.log('APIGateway handling: ' + event.RequestType);

  var onComplete = function(error, returnValue) {
    data.Error = error || undefined;
    data.Result = returnValue || undefined;
    try
    {
      response.send(event, context, data.Error ? response.FAILED : response.SUCCESS, data);
    }
    catch (e)
    {
      logResults('ALL DONE', error, returnValue);
    }
  };
  logResults('EVENT Properties', null, event.ResourceProperties);
  if (event.ResourceProperties) {
    var tasks = {};

    tasks.ensureDeleted = ensureAPIDeletedTask(event.ResourceProperties, event.OldResourceProperties || {}, data);

    if (event.RequestType !== 'Delete') {
      tasks.ensureCreated = ['ensureDeleted',
        ensureAPICreatedTask(event.ResourceProperties, data)
      ];

      tasks.ensureResources = ['ensureCreated',
        ensureResourcesCreatedTask('ensureCreated',
          event.ResourceProperties,
          data)
      ];

      tasks.ensureDeployment = ['ensureResources',
        ensureDeploymentTask('ensureCreated',
          event.ResourceProperties,
          data)
      ];
    } else {
      // TODO: Delete the old API if the name has changed
    }
    async.auto(tasks, onComplete);
  } else {
    console.log('Resource properties not found');
    response.send(event, context, response.SUCCESS, data);
  }
};
//
// var data = require('/Users/mweagle/Documents/golang/workspace/src/Sparta/resources/api.json');
// var apiData = data.Resources.APIGatewayf5eb6283ee9aab1bd72c8aac8fdd2d4a6a5a7696;
// var event = {
//   ResourceProperties: apiData.Properties,
//   RequestType: 'Create'
// };
// module.exports.handler(event, {});
